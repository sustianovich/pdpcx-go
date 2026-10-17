package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/ncruces/go-sqlite3/driver"
)

// Versión del protocolo clínico implementado en decidir(). Cambiarla cuando
// se modifique la lógica (no los parámetros) para que quede en cada decisión.
const protocoloVersion = "PROTOCOLO-2026.1 (citología 25-29, VPH-AR 30-65, triaje réflex, control 12 m)"

// ---------------------------------------------------------------------------
// Esquema (válido para SQLite y PostgreSQL; {PK} y {AHORA} se sustituyen)
// ---------------------------------------------------------------------------

const esquemaSQL = `
CREATE TABLE IF NOT EXISTS parametros (
    clave       TEXT PRIMARY KEY,
    valor       TEXT NOT NULL,
    descripcion TEXT
);

-- Cada cambio de parámetros crea una versión con la foto completa.
CREATE TABLE IF NOT EXISTS reglas_versiones (
    id            {PK},
    creada        TEXT NOT NULL DEFAULT {AHORA},
    usuario       TEXT NOT NULL,
    motivo        TEXT NOT NULL,
    protocolo     TEXT NOT NULL,
    parametros    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS mujeres (
    id               {PK},
    cip              TEXT NOT NULL UNIQUE,
    nhc              TEXT,
    fecha_nacimiento TEXT NOT NULL,
    zbs              TEXT,
    municipio        TEXT,
    cod_postal       TEXT,
    estado           TEXT NOT NULL CHECK (estado IN
                     ('PENDIENTE_EDAD','ACTIVA','SEGUIMIENTO_CLINICO',
                      'EXCLUIDA','SALIDA_EDAD','BAJA','FUSIONADA')),
    fecha_estado     TEXT NOT NULL,
    fecha_proxima    TEXT,
    motivo_proxima   TEXT,
    episodio_control BIGINT,
    fusionada_en     BIGINT,
    fecha_alta       TEXT NOT NULL,
    origen           TEXT
);
CREATE INDEX IF NOT EXISTS ix_mujeres_plan ON mujeres(estado, fecha_proxima);
CREATE INDEX IF NOT EXISTS ix_mujeres_fn   ON mujeres(fecha_nacimiento);
CREATE INDEX IF NOT EXISTS ix_mujeres_nhc  ON mujeres(nhc);

-- Registro maestro: cada cambio de un dato de la persona.
CREATE TABLE IF NOT EXISTS historial_persona (
    id        {PK},
    mujer_id  BIGINT NOT NULL REFERENCES mujeres(id),
    fecha     TEXT NOT NULL,
    campo     TEXT NOT NULL,
    anterior  TEXT,
    nuevo     TEXT,
    origen    TEXT,
    usuario   TEXT
);
CREATE INDEX IF NOT EXISTS ix_hist_mujer ON historial_persona(mujer_id);

CREATE TABLE IF NOT EXISTS vacunacion_vph (
    id       {PK},
    mujer_id BIGINT NOT NULL REFERENCES mujeres(id),
    fecha    TEXT NOT NULL,
    dosis    INTEGER,
    vacuna   TEXT,
    origen   TEXT,
    UNIQUE (mujer_id, fecha)
);

-- Catálogo de zonas básicas de salud: lo carga cada comunidad (a mano o por CSV).
CREATE TABLE IF NOT EXISTS zbs (
    codigo       TEXT PRIMARY KEY,
    nombre       TEXT NOT NULL,
    area         TEXT,
    centro       TEXT,
    municipio    TEXT,
    cupo_mensual INTEGER,
    activa       INTEGER NOT NULL DEFAULT 1,
    notas        TEXT,
    actualizada  TEXT NOT NULL DEFAULT {AHORA},
    usuario      TEXT
);

-- Días no laborables del calendario autonómico y, opcionalmente, de una ZBS.
-- zbs vacío significa que el festivo se aplica a toda la comunidad.
CREATE TABLE IF NOT EXISTS calendario_festivos (
    id           {PK},
    anio         INTEGER NOT NULL,
    fecha        TEXT NOT NULL,
    zbs          TEXT NOT NULL DEFAULT '',
    nombre       TEXT NOT NULL,
    usuario      TEXT,
    actualizada  TEXT NOT NULL DEFAULT {AHORA},
    UNIQUE (fecha, zbs)
);
CREATE INDEX IF NOT EXISTS ix_calendario_fecha ON calendario_festivos(fecha);

CREATE TABLE IF NOT EXISTS fusiones (
    id          {PK},
    fecha       TEXT NOT NULL DEFAULT {AHORA},
    principal   BIGINT NOT NULL REFERENCES mujeres(id),
    duplicada   BIGINT NOT NULL REFERENCES mujeres(id),
    cip_duplicada TEXT NOT NULL,
    usuario     TEXT NOT NULL,
    motivo      TEXT NOT NULL,
    detalle     TEXT
);

CREATE TABLE IF NOT EXISTS exclusiones (
    id           {PK},
    mujer_id     BIGINT NOT NULL REFERENCES mujeres(id),
    tipo         TEXT NOT NULL CHECK (tipo IN ('DEFINITIVA','TEMPORAL')),
    motivo       TEXT NOT NULL,
    fecha_inicio TEXT NOT NULL,
    fecha_fin    TEXT,
    origen       TEXT
);
CREATE INDEX IF NOT EXISTS ix_excl_mujer ON exclusiones(mujer_id);

CREATE TABLE IF NOT EXISTS invitaciones (
    id               {PK},
    mujer_id         BIGINT NOT NULL REFERENCES mujeres(id),
    tipo             TEXT NOT NULL CHECK (tipo IN ('CRIBADO','CONTROL')),
    episodio_origen  BIGINT REFERENCES invitaciones(id),
    ronda            INTEGER NOT NULL,
    fecha_invitacion TEXT NOT NULL,
    edad             INTEGER NOT NULL,
    prueba           TEXT NOT NULL CHECK (prueba IN ('CITOLOGIA','VPH')),
    muestra          TEXT NOT NULL CHECK (muestra IN ('PROF','AUTO')),
    motivo           TEXT NOT NULL,
    valida           INTEGER NOT NULL DEFAULT 1,
    causa_no_valida  TEXT,
    estado           TEXT NOT NULL CHECK (estado IN
                     ('ENVIADA','EN_CURSO','TRIAJE_PENDIENTE','REPETIR_MUESTRA',
                      'DERIVADA_COLPOSCOPIA','CERRADA')),
    fecha_cierre     TEXT,
    motivo_cierre    TEXT,
    fecha_proxima    TEXT,
    lote             TEXT,
    version_reglas   BIGINT
);
CREATE INDEX IF NOT EXISTS ix_inv_mujer  ON invitaciones(mujer_id);
CREATE INDEX IF NOT EXISTS ix_inv_fecha  ON invitaciones(fecha_invitacion);
CREATE INDEX IF NOT EXISTS ix_inv_estado ON invitaciones(estado);
CREATE INDEX IF NOT EXISTS ix_inv_lote   ON invitaciones(lote);

CREATE TABLE IF NOT EXISTS pruebas (
    id              {PK},
    invitacion_id   BIGINT NOT NULL REFERENCES invitaciones(id),
    mujer_id        BIGINT NOT NULL REFERENCES mujeres(id),
    tipo            TEXT NOT NULL CHECK (tipo IN ('CITOLOGIA','VPH')),
    finalidad       TEXT NOT NULL CHECK (finalidad IN ('PRIMARIA','REPETICION','TRIAJE','CONTROL')),
    muestra         TEXT NOT NULL CHECK (muestra IN ('PROF','AUTO')),
    fecha_toma      TEXT NOT NULL,
    fecha_resultado TEXT,
    resultado       TEXT NOT NULL,
    valida          INTEGER NOT NULL,
    origen          TEXT
);
CREATE INDEX IF NOT EXISTS ix_pru_inv   ON pruebas(invitacion_id);
CREATE INDEX IF NOT EXISTS ix_pru_fecha ON pruebas(fecha_toma);

CREATE TABLE IF NOT EXISTS colposcopias (
    id                {PK},
    invitacion_id     BIGINT NOT NULL REFERENCES invitaciones(id),
    mujer_id          BIGINT NOT NULL REFERENCES mujeres(id),
    fecha_derivacion  TEXT NOT NULL,
    motivo_derivacion TEXT NOT NULL,
    estado            TEXT NOT NULL CHECK (estado IN ('DERIVADA','REALIZADA','NO_ACUDE')),
    fecha_realizacion TEXT,
    adecuada          INTEGER,
    biopsia           INTEGER,
    histologia        TEXT,
    fecha_histologia  TEXT,
    origen            TEXT
);
CREATE INDEX IF NOT EXISTS ix_col_inv ON colposcopias(invitacion_id);
CREATE INDEX IF NOT EXISTS ix_col_estado ON colposcopias(estado);

-- Cada decisión del motor de reglas, con la versión que la tomó.
CREATE TABLE IF NOT EXISTS decisiones (
    id             {PK},
    registrada     TEXT NOT NULL DEFAULT {AHORA},
    fecha          TEXT NOT NULL,
    invitacion_id  BIGINT NOT NULL REFERENCES invitaciones(id),
    mujer_id       BIGINT NOT NULL REFERENCES mujeres(id),
    accion         TEXT NOT NULL,
    motivo         TEXT,
    version_reglas BIGINT NOT NULL REFERENCES reglas_versiones(id),
    protocolo      TEXT NOT NULL,
    usuario        TEXT
);
CREATE INDEX IF NOT EXISTS ix_dec_inv ON decisiones(invitacion_id);

-- Traza clínica/administrativa: todo cambio de estado de una mujer o episodio.
CREATE TABLE IF NOT EXISTS eventos (
    id            {PK},
    registrado    TEXT NOT NULL DEFAULT {AHORA},
    fecha         TEXT NOT NULL,
    mujer_id      BIGINT,
    invitacion_id BIGINT,
    tipo          TEXT NOT NULL,
    detalle       TEXT,
    origen        TEXT,
    usuario       TEXT
);
CREATE INDEX IF NOT EXISTS ix_ev_mujer ON eventos(mujer_id);
CREATE INDEX IF NOT EXISTS ix_ev_inv   ON eventos(invitacion_id);

-- Intercambios de ficheros (entradas y salidas).
CREATE TABLE IF NOT EXISTS cargas (
    id       {PK},
    fecha    TEXT NOT NULL DEFAULT {AHORA},
    tipo     TEXT NOT NULL,
    sentido  TEXT NOT NULL DEFAULT 'ENTRADA',
    fichero  TEXT,
    filas    INTEGER,
    detalle  TEXT,
    usuario  TEXT
);

-- Cada mes, incluso vacío, se genera una sola vez y en orden.
CREATE TABLE IF NOT EXISTS planificacion_mensual (
    periodo   TEXT PRIMARY KEY,
    lote      TEXT NOT NULL UNIQUE,
    fecha     TEXT NOT NULL DEFAULT {AHORA},
    usuario   TEXT NOT NULL,
    cerradas  INTEGER NOT NULL,
    aplazadas INTEGER NOT NULL,
    salidas   INTEGER NOT NULL
);
-- Serializa la generación entre conexiones y procesos en ambos motores.
CREATE TABLE IF NOT EXISTS planificacion_control (id INTEGER PRIMARY KEY CHECK (id=1));
INSERT INTO planificacion_control(id) VALUES(1) ON CONFLICT (id) DO NOTHING;

-- Auditoría de accesos y acciones (quién, qué, cuándo, desde dónde).
CREATE TABLE IF NOT EXISTS auditoria (
    id       {PK},
    ts       TEXT NOT NULL DEFAULT {AHORA},
    usuario  TEXT NOT NULL,
    rol      TEXT,
    canal    TEXT NOT NULL,
    accion   TEXT NOT NULL,
    objeto   TEXT,
    detalle  TEXT,
    ip       TEXT
);
CREATE INDEX IF NOT EXISTS ix_aud_ts ON auditoria(ts);
CREATE INDEX IF NOT EXISTS ix_aud_usuario ON auditoria(usuario);

CREATE TABLE IF NOT EXISTS usuarios (
    id                {PK},
    login             TEXT NOT NULL UNIQUE,
    nombre            TEXT NOT NULL,
    rol               TEXT NOT NULL CHECK (rol IN
                      ('ADMIN','COORDINACION','ADMISION','AP','GINECOLOGIA','LABORATORIO','CONSULTA')),
    hash              TEXT NOT NULL,
    activo            INTEGER NOT NULL DEFAULT 1,
    intentos_fallidos INTEGER NOT NULL DEFAULT 0,
    bloqueado_hasta   TEXT,
    debe_cambiar      INTEGER NOT NULL DEFAULT 1,
    creado            TEXT NOT NULL DEFAULT {AHORA},
    ultimo_acceso     TEXT
);

CREATE TABLE IF NOT EXISTS sesiones (
    token_hash TEXT PRIMARY KEY,
    usuario_id BIGINT NOT NULL REFERENCES usuarios(id),
    creada     TEXT NOT NULL,
    ultima     TEXT NOT NULL,
    csrf       TEXT NOT NULL,
    ip         TEXT
);
`

var parametrosDefecto = []struct{ clave, valor, desc string }{
	{"edad_cito_min", "25", "Edad mínima de cribado con citología"},
	{"edad_cito_max", "29", "Edad máxima de cribado con citología"},
	{"edad_vph_min", "30", "Edad mínima de cribado con VPH-AR"},
	{"edad_vph_max", "65", "Edad máxima de cribado con VPH-AR (incluida)"},
	{"intervalo_cito_meses", "36", "Intervalo tras citología negativa"},
	{"intervalo_vph_meses", "60", "Intervalo tras VPH negativo"},
	{"control_meses", "12", "Re-cribado precoz tras triaje negativo (DECISIÓN CLÍNICA)"},
	{"meses_tras_alta_colposcopia", "12", "Vuelta a cribado tras colposcopia sin CIN2+ (DECISIÓN CLÍNICA, pendiente de comité)"},
	{"meses_tras_no_responde", "12", "Reinvitación de no respondedoras (DECISIÓN DE PROGRAMA)"},
	{"meses_tras_no_valida", "6", "Reintento tras invitación no válida"},
	{"plazo_respuesta_dias", "180", "Días sin muestra tras los que la invitación se cierra como NO_RESPONDE"},
	{"max_repeticiones", "2", "Repeticiones de muestra no válida antes de cerrar"},
	{"fecha_inicio_programa", "2024-01-01", "Inicio del programa: base para repartir la ronda inicial"},
	{"autotoma_inicio", "", "Fecha desde la que se ofrece autotoma (vacío = desactivada)"},
	{"autotoma_estrategia", "NO_RESPONDEDORAS", "TODAS | NO_RESPONDEDORAS (solo VPH)"},
	{"ine_poblacion_cito", "", "Población INE de mujeres en edad de citología a 01/01 (celda C4)"},
	{"ine_poblacion_vph", "", "Población INE de mujeres en edad de VPH a 01/01 (celda D4)"},
	{"plazo_recordatorio_dias", "60", "Lista viva: invitación sin respuesta -> recordatorio (Admisión)"},
	{"plazo_ap_dias", "30", "Lista viva: plazo para citar toma o repetición (AP)"},
	{"plazo_laboratorio_dias", "15", "Lista viva: plazo del triaje réflex (Laboratorio)"},
	{"plazo_colposcopia_dias", "60", "Lista viva: plazo derivación -> colposcopia (Ginecología)"},
	{"plazo_histologia_dias", "30", "Lista viva: plazo colposcopia -> histología (Ginecología)"},
	{"meses_valorar_reincorporacion", "24", "Lista viva: seguimiento clínico a valorar (Coordinación)"},
	{"sesion_minutos", "60", "Web: minutos de inactividad que cierran la sesión"},
	{"max_intentos_login", "5", "Web: intentos fallidos antes de bloquear 15 minutos"},
}

// ---------------------------------------------------------------------------
// Acceso a datos: SQLite (fichero) o PostgreSQL (servidor) con el mismo SQL
// ---------------------------------------------------------------------------

type BD struct {
	*sql.DB
	pg      bool
	mu      sync.RWMutex      // protege par y version (la web es concurrente)
	par     map[string]string // usar getPar()
	version int64             // versión de reglas vigente: usar verReglas()
}

func (b *BD) getPar(k string) string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.par[k]
}

func (b *BD) verReglas() int64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.version
}

func (b *BD) copiaPar() map[string]string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	m := make(map[string]string, len(b.par))
	for k, v := range b.par {
		m[k] = v
	}
	return m
}

type Tx struct {
	*sql.Tx
	b *BD
}

// q traduce los marcadores '?' a '$n' en PostgreSQL (respetando literales).
func (b *BD) q(s string) string {
	if !b.pg {
		return s
	}
	var sb strings.Builder
	n, enLit := 0, false
	for _, c := range s {
		switch {
		case c == '\'':
			enLit = !enLit
			sb.WriteRune(c)
		case c == '?' && !enLit:
			n++
			sb.WriteString("$" + strconv.Itoa(n))
		default:
			sb.WriteRune(c)
		}
	}
	return sb.String()
}

func (b *BD) Exec(s string, a ...any) (sql.Result, error) { return b.DB.Exec(b.q(s), a...) }
func (b *BD) Query(s string, a ...any) (*sql.Rows, error) { return b.DB.Query(b.q(s), a...) }
func (b *BD) QueryRow(s string, a ...any) *sql.Row        { return b.DB.QueryRow(b.q(s), a...) }
func (b *BD) Begin() (*Tx, error) {
	t, err := b.DB.Begin()
	return &Tx{t, b}, err
}
func (t *Tx) Exec(s string, a ...any) (sql.Result, error) { return t.Tx.Exec(t.b.q(s), a...) }
func (t *Tx) Query(s string, a ...any) (*sql.Rows, error) { return t.Tx.Query(t.b.q(s), a...) }
func (t *Tx) QueryRow(s string, a ...any) *sql.Row        { return t.Tx.QueryRow(t.b.q(s), a...) }
func (t *Tx) Prepare(s string) (*sql.Stmt, error)         { return t.Tx.Prepare(t.b.q(s)) }

type consultor interface {
	QueryRow(s string, a ...any) *sql.Row
}

// insertar ejecuta un INSERT y devuelve el id (RETURNING funciona en ambos motores).
func insertar(x consultor, s string, a ...any) (int64, error) {
	var id int64
	err := x.QueryRow(s+" RETURNING id", a...).Scan(&id)
	return id, err
}

func esPostgres(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}

// abrirBD acepta un fichero SQLite (p. ej. cervix.db) o una URL PostgreSQL
// (postgres://usuario:clave@servidor:5432/base?sslmode=require).
func abrirBD(dsn string) (*BD, error) {
	b := &BD{pg: esPostgres(dsn)}
	var err error
	if b.pg {
		b.DB, err = sql.Open("pgx", dsn)
		if err == nil {
			b.DB.SetMaxOpenConns(20)
			b.DB.SetConnMaxIdleTime(5 * time.Minute)
		}
	} else {
		// DELETE journal mode avoids the WAL shared-memory index used by SQLite.
		// The bundled ncruces SQLite can panic while opening a WAL database on
		// Windows, leaving the web server unable to start.
		b.DB, err = sql.Open("sqlite3", "file:"+dsn+"?_pragma=foreign_keys(1)&_pragma=journal_mode(DELETE)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(10000)")
		if err == nil {
			b.DB.SetMaxOpenConns(1)
		}
	}
	if err != nil {
		return nil, err
	}
	if err := b.Ping(); err != nil {
		return nil, fmt.Errorf("conectando con la base de datos: %w", err)
	}
	pk, ahora := "INTEGER PRIMARY KEY", "(strftime('%Y-%m-%d %H:%M:%S','now','localtime'))"
	if b.pg {
		pk, ahora = "BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY", "(to_char(now(),'YYYY-MM-DD HH24:MI:SS'))"
	}
	ddl := strings.NewReplacer("{PK}", pk, "{AHORA}", ahora).Replace(esquemaSQL)
	for _, st := range strings.Split(ddl, ";\n") {
		if strings.TrimSpace(st) == "" {
			continue
		}
		if _, err := b.DB.Exec(st); err != nil {
			return nil, fmt.Errorf("creando esquema: %w\n%s", err, st)
		}
	}
	for _, p := range parametrosDefecto {
		if _, err := b.Exec(`INSERT INTO parametros(clave,valor,descripcion) VALUES(?,?,?) ON CONFLICT (clave) DO NOTHING`, p.clave, p.valor, p.desc); err != nil {
			return nil, err
		}
	}
	if err := b.cargarParametros(); err != nil {
		return nil, err
	}
	if b.verReglas() == 0 {
		if _, err := b.nuevaVersion(nil, "sistema", "Versión inicial"); err != nil {
			return nil, err
		}
		if err := b.cargarParametros(); err != nil {
			return nil, err
		}
	}
	return b, nil
}

func (b *BD) cargarParametros() error {
	m := map[string]string{}
	rows, err := b.Query(`SELECT clave, valor FROM parametros`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			rows.Close()
			return err
		}
		m[k] = v
	}
	rows.Close()
	var ver int64
	if err := b.QueryRow(`SELECT COALESCE(MAX(id),0) FROM reglas_versiones`).Scan(&ver); err != nil {
		return err
	}
	b.mu.Lock()
	b.par, b.version = m, ver
	b.mu.Unlock()
	return nil
}

func (b *BD) pInt(clave string) int {
	v := b.getPar(clave)
	n, err := strconv.Atoi(v)
	if err != nil {
		panic(fmt.Sprintf("parámetro %s no numérico: %q", clave, v))
	}
	return n
}

func (b *BD) pFecha(clave string) (time.Time, bool) {
	v := b.getPar(clave)
	if v == "" {
		return time.Time{}, false
	}
	return mustFecha(v), true
}

// nuevaVersion guarda una foto de todos los parámetros como versión de reglas.
func (b *BD) nuevaVersion(tx *Tx, usuario, motivo string) (int64, error) {
	foto, _ := json.Marshal(b.copiaPar())
	var x consultor = b
	if tx != nil {
		x = tx
	}
	return insertar(x, `INSERT INTO reglas_versiones(usuario,motivo,protocolo,parametros) VALUES(?,?,?,?)`,
		usuario, motivo, protocoloVersion, string(foto))
}

// fijarParametro valida, cambia el parámetro, crea una versión nueva de las
// reglas y lo deja en auditoría.
func (b *BD) fijarParametro(clave, valor, usuario, canal, motivo string) error {
	ant, ok := b.copiaPar()[clave]
	if !ok {
		return fmt.Errorf("parámetro desconocido: %s", clave)
	}
	if strings.TrimSpace(motivo) == "" {
		return fmt.Errorf("indique el motivo del cambio")
	}
	switch {
	case strings.HasSuffix(clave, "_inicio") || strings.HasPrefix(clave, "fecha_"):
		if valor != "" {
			if _, err := time.Parse(fISO, valor); err != nil {
				return fmt.Errorf("%s debe ser una fecha AAAA-MM-DD o vacío", clave)
			}
		}
	case clave == "autotoma_estrategia":
		if valor != "TODAS" && valor != "NO_RESPONDEDORAS" {
			return fmt.Errorf("autotoma_estrategia: TODAS o NO_RESPONDEDORAS")
		}
	case strings.HasPrefix(clave, "ine_"):
		if valor != "" {
			if _, err := strconv.Atoi(valor); err != nil {
				return fmt.Errorf("%s debe ser un número entero o vacío", clave)
			}
		}
	default:
		if n, err := strconv.Atoi(valor); err != nil || n < 0 {
			return fmt.Errorf("%s debe ser un entero no negativo", clave)
		}
	}
	if ant == valor {
		return fmt.Errorf("%s ya vale %q", clave, valor)
	}
	tx, err := b.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE parametros SET valor=? WHERE clave=?`, valor, clave); err != nil {
		return err
	}
	nuevo := b.copiaPar()
	nuevo[clave] = valor
	foto, _ := json.Marshal(nuevo)
	v, err := insertar(tx, `INSERT INTO reglas_versiones(usuario,motivo,protocolo,parametros) VALUES(?,?,?,?)`,
		usuario, fmt.Sprintf("%s: %q -> %q. %s", clave, ant, valor, motivo), protocoloVersion, string(foto))
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	b.mu.Lock()
	b.par, b.version = nuevo, v
	b.mu.Unlock()
	b.auditar(usuario, "", canal, "PARAMETRO_CAMBIADO", clave, fmt.Sprintf("%q -> %q (versión %d): %s", ant, valor, v, motivo), "")
	return nil
}

// auditar no debe impedir la operación si falla, pero sí avisar.
func (b *BD) auditar(usuario, rol, canal, accion, objeto, detalle, ip string) {
	if _, err := b.Exec(`INSERT INTO auditoria(usuario,rol,canal,accion,objeto,detalle,ip) VALUES(?,?,?,?,?,?,?)`,
		usuario, rol, canal, accion, objeto, detalle, ip); err != nil {
		fmt.Printf("AVISO: no se pudo auditar %s: %v\n", accion, err)
	}
}

// ---------------------------------------------------------------------------
// Fechas
// ---------------------------------------------------------------------------

const fISO = "2006-01-02"

func mustFecha(s string) time.Time {
	t, err := time.Parse(fISO, s)
	if err != nil {
		panic(fmt.Sprintf("fecha no válida %q", s))
	}
	return t
}

func parseFecha(s string) (time.Time, error) {
	for _, l := range []string{fISO, "02/01/2006", "2/1/2006"} {
		if t, err := time.Parse(l, strings.TrimSpace(s)); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("fecha no válida %q (use AAAA-MM-DD o DD/MM/AAAA)", s)
}

func fs(t time.Time) string { return t.Format(fISO) }

func nullFecha(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return fs(t)
}

func hoy() time.Time {
	n := time.Now()
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
}

func edad(nac, f time.Time) int {
	e := f.Year() - nac.Year()
	if f.Month() < nac.Month() || (f.Month() == nac.Month() && f.Day() < nac.Day()) {
		e--
	}
	return e
}

func cumple(nac time.Time, n int) time.Time {
	y := nac.Year() + n
	d := nac.Day()
	if nac.Month() == time.February && d == 29 && !bisiesto(y) {
		d = 28
	}
	return time.Date(y, nac.Month(), d, 0, 0, 0, 0, time.UTC)
}

func bisiesto(y int) bool { return y%4 == 0 && (y%100 != 0 || y%400 == 0) }

func addMeses(t time.Time, m int) time.Time { return t.AddDate(0, m, 0) }

func maxT(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// Traza
// ---------------------------------------------------------------------------

type ejecutor interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func evento(x ejecutor, fecha time.Time, mujerID, invID int64, tipo, detalle, origen, usuario string) error {
	var inv any
	if invID > 0 {
		inv = invID
	}
	_, err := x.Exec(`INSERT INTO eventos(fecha,mujer_id,invitacion_id,tipo,detalle,origen,usuario) VALUES(?,?,?,?,?,?,?)`,
		fs(fecha), mujerID, inv, tipo, detalle, origen, usuario)
	return err
}

// Programa = BD + quién actúa. Todas las operaciones del programa cuelgan de aquí.
type Programa struct {
	*BD
	Usuario string
	Canal   string // CLI, WEB, SIMULADOR
}

func (pg Programa) ev(x ejecutor, fecha time.Time, mujerID, invID int64, tipo, detalle, origen string) error {
	return evento(x, fecha, mujerID, invID, tipo, detalle, origen, pg.Usuario)
}

func (pg Programa) registrarCarga(x ejecutor, tipo, sentido, fichero string, filas int, detalle string) error {
	_, err := x.Exec(`INSERT INTO cargas(tipo,sentido,fichero,filas,detalle,usuario) VALUES(?,?,?,?,?,?)`,
		tipo, sentido, fichero, filas, detalle, pg.Usuario)
	return err
}
