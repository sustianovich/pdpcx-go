package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

var roles = []string{"ADMIN", "COORDINACION", "ADMISION", "AP", "GINECOLOGIA", "LABORATORIO", "CONSULTA"}

const iterPBKDF2 = 600000

type Usuario struct {
	ID          int64
	Login       string
	Nombre      string
	Rol         string
	Activo      bool
	DebeCambiar bool
	Ultimo      string
	Bloqueado   string
}

func hashClave(clave string) (string, error) {
	sal := make([]byte, 16)
	if _, err := rand.Read(sal); err != nil {
		return "", err
	}
	k, err := pbkdf2.Key(sha256.New, clave, sal, iterPBKDF2, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", iterPBKDF2,
		base64.RawStdEncoding.EncodeToString(sal), base64.RawStdEncoding.EncodeToString(k)), nil
}

func verificarClave(hash, clave string) bool {
	p := strings.Split(hash, "$")
	if len(p) != 4 || p[0] != "pbkdf2-sha256" {
		return false
	}
	it, err := strconv.Atoi(p[1])
	if err != nil {
		return false
	}
	sal, e1 := base64.RawStdEncoding.DecodeString(p[2])
	esp, e2 := base64.RawStdEncoding.DecodeString(p[3])
	if e1 != nil || e2 != nil {
		return false
	}
	k, err := pbkdf2.Key(sha256.New, clave, sal, it, len(esp))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(k, esp) == 1
}

// Política mínima: 12 caracteres, con letras y números.
func validarClave(c string) error {
	if len([]rune(c)) < 12 {
		return fmt.Errorf("la contraseña debe tener al menos 12 caracteres")
	}
	var l, d bool
	for _, r := range c {
		l = l || unicode.IsLetter(r)
		d = d || unicode.IsDigit(r)
	}
	if !l || !d {
		return fmt.Errorf("la contraseña debe combinar letras y números")
	}
	return nil
}

func rolValido(r string) bool {
	for _, x := range roles {
		if x == r {
			return true
		}
	}
	return false
}

func ahoraTS() string { return time.Now().Format("2006-01-02 15:04:05") }

func (b *BD) crearUsuario(login, nombre, rol, clave, creador, canal string) error {
	login = strings.ToLower(strings.TrimSpace(login))
	if login == "" || nombre == "" {
		return fmt.Errorf("login y nombre son obligatorios")
	}
	if !rolValido(rol) {
		return fmt.Errorf("rol no válido %q (%s)", rol, strings.Join(roles, ", "))
	}
	if err := validarClave(clave); err != nil {
		return err
	}
	h, err := hashClave(clave)
	if err != nil {
		return err
	}
	if _, err := b.Exec(`INSERT INTO usuarios(login,nombre,rol,hash,debe_cambiar) VALUES(?,?,?,?,1)`, login, nombre, rol, h); err != nil {
		return fmt.Errorf("creando usuario %s: %w", login, err)
	}
	b.auditar(creador, "", canal, "USUARIO_CREADO", login, "rol "+rol, "")
	return nil
}

func (b *BD) usuarioPorID(id int64) (*Usuario, error) {
	u := &Usuario{}
	var act, dc int
	var ult, bloq sql.NullString
	err := b.QueryRow(`SELECT id, login, nombre, rol, activo, debe_cambiar, ultimo_acceso, bloqueado_hasta FROM usuarios WHERE id=?`, id).
		Scan(&u.ID, &u.Login, &u.Nombre, &u.Rol, &act, &dc, &ult, &bloq)
	u.Activo, u.DebeCambiar, u.Ultimo, u.Bloqueado = act == 1, dc == 1, ult.String, bloq.String
	return u, err
}

func (b *BD) listarUsuarios() ([]Usuario, error) {
	rows, err := b.Query(`SELECT id, login, nombre, rol, activo, debe_cambiar, COALESCE(ultimo_acceso,''), COALESCE(bloqueado_hasta,'') FROM usuarios ORDER BY login`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Usuario
	for rows.Next() {
		var u Usuario
		var a, d int
		if err := rows.Scan(&u.ID, &u.Login, &u.Nombre, &u.Rol, &a, &d, &u.Ultimo, &u.Bloqueado); err != nil {
			return nil, err
		}
		u.Activo, u.DebeCambiar = a == 1, d == 1
		out = append(out, u)
	}
	return out, rows.Err()
}

var (
	onceFicticio sync.Once
	hashFicticio string
)

func ficticio() string {
	onceFicticio.Do(func() { hashFicticio, _ = hashClave("clave-ficticia-para-igualar-tiempos-1") })
	return hashFicticio
}

// autenticar comprueba usuario y clave con bloqueo tras N fallos (15 min).
func (b *BD) autenticar(login, clave, ip string) (*Usuario, error) {
	login = strings.ToLower(strings.TrimSpace(login))
	var id int64
	var hash string
	var activo, fallos int
	var bloq sql.NullString
	err := b.QueryRow(`SELECT id, hash, activo, intentos_fallidos, bloqueado_hasta FROM usuarios WHERE login=?`, login).
		Scan(&id, &hash, &activo, &fallos, &bloq)
	errGen := fmt.Errorf("usuario o contraseña incorrectos")
	if err == sql.ErrNoRows {
		verificarClave(ficticio(), clave) // mismo tiempo de respuesta
		b.auditar(login, "", "WEB", "LOGIN_FALLIDO", login, "usuario inexistente", ip)
		return nil, errGen
	}
	if err != nil {
		return nil, err
	}
	if bloq.Valid && bloq.String > ahoraTS() {
		b.auditar(login, "", "WEB", "LOGIN_BLOQUEADO", login, "bloqueado hasta "+bloq.String, ip)
		return nil, fmt.Errorf("usuario bloqueado temporalmente por intentos fallidos; inténtelo más tarde")
	}
	if activo != 1 {
		b.auditar(login, "", "WEB", "LOGIN_FALLIDO", login, "usuario desactivado", ip)
		return nil, errGen
	}
	if !verificarClave(hash, clave) {
		fallos++
		if fallos >= b.pInt("max_intentos_login") {
			b.Exec(`UPDATE usuarios SET intentos_fallidos=0, bloqueado_hasta=? WHERE id=?`, time.Now().Add(15*time.Minute).Format("2006-01-02 15:04:05"), id)
			b.auditar(login, "", "WEB", "LOGIN_BLOQUEADO", login, fmt.Sprintf("%d intentos fallidos", fallos), ip)
		} else {
			b.Exec(`UPDATE usuarios SET intentos_fallidos=? WHERE id=?`, fallos, id)
			b.auditar(login, "", "WEB", "LOGIN_FALLIDO", login, "contraseña incorrecta", ip)
		}
		return nil, errGen
	}
	b.Exec(`UPDATE usuarios SET intentos_fallidos=0, bloqueado_hasta=NULL, ultimo_acceso=? WHERE id=?`, ahoraTS(), id)
	u, err := b.usuarioPorID(id)
	if err == nil {
		b.auditar(u.Login, u.Rol, "WEB", "LOGIN_OK", u.Login, "", ip)
	}
	return u, err
}

func (b *BD) cambiarClave(id int64, nueva string, obligarCambio bool, actor, canal string) error {
	if err := validarClave(nueva); err != nil {
		return err
	}
	h, err := hashClave(nueva)
	if err != nil {
		return err
	}
	dc := 0
	if obligarCambio {
		dc = 1
	}
	if _, err := b.Exec(`UPDATE usuarios SET hash=?, debe_cambiar=?, intentos_fallidos=0, bloqueado_hasta=NULL WHERE id=?`, h, dc, id); err != nil {
		return err
	}
	// Cambiar la clave cierra todas las sesiones abiertas de ese usuario.
	b.Exec(`DELETE FROM sesiones WHERE usuario_id=?`, id)
	u, _ := b.usuarioPorID(id)
	acc := "CLAVE_CAMBIADA"
	if obligarCambio {
		acc = "CLAVE_RESETEADA"
	}
	b.auditar(actor, "", canal, acc, u.Login, "", "")
	return nil
}

// ---------------------------------------------------------------------------
// Sesiones: el navegador guarda un token aleatorio; la BD solo su SHA-256.
// ---------------------------------------------------------------------------

func aleatorio(n int) string {
	x := make([]byte, n)
	rand.Read(x)
	return base64.RawURLEncoding.EncodeToString(x)
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

type Sesion struct {
	Usuario *Usuario
	CSRF    string
	token   string
}

func (b *BD) crearSesion(uid int64, ip string) (string, error) {
	tok := aleatorio(32)
	_, err := b.Exec(`INSERT INTO sesiones(token_hash,usuario_id,creada,ultima,csrf,ip) VALUES(?,?,?,?,?,?)`,
		hashToken(tok), uid, ahoraTS(), ahoraTS(), aleatorio(32), ip)
	return tok, err
}

func (b *BD) sesion(tok string) (*Sesion, error) {
	var uid int64
	var ultima, csrf string
	err := b.QueryRow(`SELECT usuario_id, ultima, csrf FROM sesiones WHERE token_hash=?`, hashToken(tok)).Scan(&uid, &ultima, &csrf)
	if err != nil {
		return nil, err
	}
	lim := time.Now().Add(-time.Duration(b.pInt("sesion_minutos")) * time.Minute).Format("2006-01-02 15:04:05")
	if ultima < lim {
		b.Exec(`DELETE FROM sesiones WHERE token_hash=?`, hashToken(tok))
		return nil, sql.ErrNoRows
	}
	u, err := b.usuarioPorID(uid)
	if err != nil || !u.Activo {
		return nil, sql.ErrNoRows
	}
	b.Exec(`UPDATE sesiones SET ultima=? WHERE token_hash=?`, ahoraTS(), hashToken(tok))
	return &Sesion{Usuario: u, CSRF: csrf, token: tok}, nil
}

func (b *BD) cerrarSesion(tok string) {
	b.Exec(`DELETE FROM sesiones WHERE token_hash=?`, hashToken(tok))
}
