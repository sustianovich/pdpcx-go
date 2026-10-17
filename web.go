package main

import (
	"crypto/subtle"
	"embed"
	"encoding/csv"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed web/plantillas.html web/estilo.css
var webFS embed.FS

// Permisos por acción. Los datos clínicos individuales (ficha) no son
// visibles para CONSULTA: ese rol solo ve indicadores agregados.
var permisos = map[string][]string{
	"ficha":        {"ADMIN", "COORDINACION", "ADMISION", "AP", "GINECOLOGIA", "LABORATORIO"},
	"prueba":       {"ADMIN", "COORDINACION", "AP", "LABORATORIO"},
	"colposcopia":  {"ADMIN", "COORDINACION", "GINECOLOGIA"},
	"exclusion":    {"ADMIN", "COORDINACION", "ADMISION"},
	"novalida":     {"ADMIN", "COORDINACION", "ADMISION"},
	"reincorporar": {"ADMIN", "COORDINACION"},
	"fusionar":     {"ADMIN", "COORDINACION"},
	"pendientes":   {"ADMIN", "COORDINACION", "ADMISION", "AP", "GINECOLOGIA", "LABORATORIO"},
	"kpis":         {"ADMIN", "COORDINACION", "CONSULTA"},
	"lotes":        {"ADMIN", "COORDINACION"},
	"importar":     {"ADMIN", "COORDINACION", "LABORATORIO"},
	"parametros":   {"ADMIN", "COORDINACION"},
	"zonas":        {"ADMIN", "COORDINACION"},
	"usuarios":     {"ADMIN"},
	"auditoria":    {"ADMIN"},
}

func puede(s *Sesion, accion string) bool {
	if s == nil || s.Usuario == nil {
		return false
	}
	for _, r := range permisos[accion] {
		if r == s.Usuario.Rol {
			return true
		}
	}
	return false
}

// Servicio de la lista viva que corresponde a cada rol.
func servicioDeRol(rol string) string {
	switch rol {
	case "ADMISION", "AP", "GINECOLOGIA", "LABORATORIO":
		return rol
	}
	return ""
}

type Servidor struct {
	b            *BD
	tmpl         *template.Template
	cookieSegura bool
	confiarProxy bool
}

type Pagina struct {
	Titulo, Msg, Err string
	NavActiva        string
	S                *Sesion
	D                any
}

const nombreCookie = "pdpcx_sesion"

func nuevoServidor(b *BD, cookieSegura, confiarProxy bool) (*Servidor, error) {
	t, err := template.New("").Funcs(template.FuncMap{
		"tiene": puede,
		"fecha": func(t time.Time) string { return fs(t) },
		"deref": func(p *int) int {
			if p == nil {
				return 0
			}
			return *p
		},
	}).ParseFS(webFS, "web/plantillas.html")
	if err != nil {
		return nil, err
	}
	return &Servidor{b: b, tmpl: t, cookieSegura: cookieSegura, confiarProxy: confiarProxy}, nil
}

func (sv *Servidor) ip(r *http.Request) string {
	if sv.confiarProxy {
		if x := r.Header.Get("X-Forwarded-For"); x != "" {
			return strings.TrimSpace(strings.Split(x, ",")[0])
		}
	}
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

func (sv *Servidor) aud(r *http.Request, s *Sesion, accion, objeto, detalle string) {
	u, rol := "-", ""
	if s != nil && s.Usuario != nil {
		u, rol = s.Usuario.Login, s.Usuario.Rol
	}
	sv.b.auditar(u, rol, "WEB", accion, objeto, detalle, sv.ip(r))
}

func (sv *Servidor) pg(s *Sesion) Programa {
	return Programa{BD: sv.b, Usuario: s.Usuario.Login, Canal: "WEB"}
}

// Solo las plantillas de sección seleccionan un enlace de la cabecera.
var navegacionPorPlantilla = map[string]string{
	"inicio":        "/",
	"buscar":        "/buscar",
	"ficha":         "/buscar",
	"pendientes":    "/pendientes",
	"kpis":          "/kpis",
	"lotes":         "/lotes",
	"planificacion": "/planificacion",
	"importar":      "/importar",
	"zonas":         "/zonas",
	"parametros":    "/parametros",
	"usuarios":      "/usuarios",
	"auditoria":     "/auditoria",
}

func (sv *Servidor) render(w http.ResponseWriter, nombre string, p Pagina) {
	p.NavActiva = navegacionPorPlantilla[nombre]
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := sv.tmpl.ExecuteTemplate(w, nombre, p); err != nil {
		log.Printf("plantilla %s: %v", nombre, err)
	}
}

func (sv *Servidor) fallo(w http.ResponseWriter, s *Sesion, codigo int, msg string) {
	w.WriteHeader(codigo)
	sv.render(w, "error", Pagina{Titulo: msg, S: s})
}

// cabeceras de seguridad comunes a todas las respuestas
func cabeceras(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

type manejador func(w http.ResponseWriter, r *http.Request, s *Sesion)

// proteger: exige sesión válida, el permiso indicado y token CSRF en los POST.
func (sv *Servidor) proteger(permiso string, h manejador) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(nombreCookie)
		if err != nil {
			http.Redirect(w, r, "/entrar", http.StatusSeeOther)
			return
		}
		s, err := sv.b.sesion(c.Value)
		if err != nil {
			http.SetCookie(w, &http.Cookie{Name: nombreCookie, Value: "", Path: "/", MaxAge: -1})
			http.Redirect(w, r, "/entrar", http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost {
			if r.Header.Get("Content-Type") != "" && strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
				r.ParseMultipartForm(50 << 20)
			} else {
				r.ParseForm()
			}
			if subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(s.CSRF)) != 1 {
				sv.aud(r, s, "CSRF_RECHAZADO", r.URL.Path, "")
				sv.fallo(w, s, http.StatusForbidden, "Petición rechazada (token de seguridad no válido). Recargue la página.")
				return
			}
		}
		// Contraseña provisional: solo puede cambiarla o salir.
		if s.Usuario.DebeCambiar && r.URL.Path != "/clave" && r.URL.Path != "/salir" {
			http.Redirect(w, r, "/clave", http.StatusSeeOther)
			return
		}
		if permiso != "" && !puede(s, permiso) {
			sv.aud(r, s, "ACCESO_DENEGADO", r.URL.Path, "permiso "+permiso)
			sv.fallo(w, s, http.StatusForbidden, "No tiene permiso para esta acción.")
			return
		}
		h(w, r, s)
	}
}

func (sv *Servidor) rutas() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /estatico/estilo.css", func(w http.ResponseWriter, r *http.Request) {
		b, _ := webFS.ReadFile("web/estilo.css")
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Write(b)
	})
	m.HandleFunc("GET /entrar", func(w http.ResponseWriter, r *http.Request) { sv.render(w, "login", Pagina{Titulo: "Acceso"}) })
	m.HandleFunc("POST /entrar", sv.entrar)
	m.HandleFunc("POST /salir", sv.proteger("", sv.salir))
	m.HandleFunc("GET /clave", sv.proteger("", func(w http.ResponseWriter, r *http.Request, s *Sesion) {
		sv.render(w, "clave", Pagina{Titulo: "Contraseña", S: s, D: s.Usuario.DebeCambiar})
	}))
	m.HandleFunc("POST /clave", sv.proteger("", sv.cambiarClaveWeb))
	m.HandleFunc("GET /{$}", sv.proteger("", sv.inicio))
	m.HandleFunc("GET /buscar", sv.proteger("ficha", sv.buscar))
	m.HandleFunc("GET /mujer/{id}", sv.proteger("ficha", sv.ficha))
	m.HandleFunc("POST /mujer/{id}/prueba", sv.proteger("prueba", sv.accionPrueba))
	m.HandleFunc("POST /mujer/{id}/colposcopia", sv.proteger("colposcopia", sv.accionColposcopia))
	m.HandleFunc("POST /mujer/{id}/exclusion", sv.proteger("exclusion", sv.accionExclusion))
	m.HandleFunc("POST /mujer/{id}/novalida", sv.proteger("novalida", sv.accionNoValida))
	m.HandleFunc("POST /mujer/{id}/reincorporar", sv.proteger("reincorporar", sv.accionReincorporar))
	m.HandleFunc("POST /mujer/{id}/fusionar", sv.proteger("fusionar", sv.accionFusionar))
	m.HandleFunc("GET /pendientes", sv.proteger("pendientes", sv.pendientesWeb))
	m.HandleFunc("GET /kpis", sv.proteger("kpis", sv.kpisWeb))
	m.HandleFunc("GET /kpis.csv", sv.proteger("kpis", sv.kpisCSV))
	m.HandleFunc("GET /lotes", sv.proteger("lotes", sv.lotes))
	m.HandleFunc("GET /planificacion", sv.proteger("lotes", sv.planificacionMensualWeb))
	m.HandleFunc("POST /planificacion/generar", sv.proteger("lotes", sv.generarMesWeb))
	m.HandleFunc("POST /lotes/planificar", sv.proteger("lotes", sv.planificarWeb))
	m.HandleFunc("GET /lotes/exportar", sv.proteger("lotes", sv.exportarWeb))
	m.HandleFunc("GET /importar", sv.proteger("importar", sv.importarForm))
	m.HandleFunc("POST /importar", sv.proteger("importar", sv.importarWeb))
	m.HandleFunc("GET /zonas", sv.proteger("zonas", sv.zonasWeb))
	m.HandleFunc("POST /zonas", sv.proteger("zonas", sv.zonaGuardar))
	m.HandleFunc("POST /zonas/importar", sv.proteger("zonas", sv.zonasImportar))
	m.HandleFunc("POST /zonas/padron", sv.proteger("zonas", sv.zonasPadron))
	m.HandleFunc("GET /zonas.csv", sv.proteger("zonas", sv.zonasCSV))
	m.HandleFunc("GET /parametros", sv.proteger("parametros", sv.parametrosWeb))
	m.HandleFunc("POST /parametros", sv.proteger("parametros", sv.parametroCambiar))
	m.HandleFunc("GET /usuarios", sv.proteger("usuarios", sv.usuariosWeb))
	m.HandleFunc("POST /usuarios", sv.proteger("usuarios", sv.usuarioCrear))
	m.HandleFunc("POST /usuarios/{id}/activo", sv.proteger("usuarios", sv.usuarioActivo))
	m.HandleFunc("POST /usuarios/{id}/clave", sv.proteger("usuarios", sv.usuarioClave))
	m.HandleFunc("GET /auditoria", sv.proteger("auditoria", sv.auditoriaWeb))
	return cabeceras(m)
}

// ---------------------------------------------------------------------------
// Acceso
// ---------------------------------------------------------------------------

func (sv *Servidor) entrar(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	u, err := sv.b.autenticar(r.PostFormValue("login"), r.PostFormValue("clave"), sv.ip(r))
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		sv.render(w, "login", Pagina{Titulo: "Acceso", Err: err.Error()})
		return
	}
	tok, err := sv.b.crearSesion(u.ID, sv.ip(r))
	if err != nil {
		sv.fallo(w, nil, 500, "Error creando la sesión")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: nombreCookie, Value: tok, Path: "/", HttpOnly: true,
		Secure: sv.cookieSegura || r.TLS != nil, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (sv *Servidor) salir(w http.ResponseWriter, r *http.Request, s *Sesion) {
	sv.b.cerrarSesion(s.token)
	sv.aud(r, s, "LOGOUT", s.Usuario.Login, "")
	http.SetCookie(w, &http.Cookie{Name: nombreCookie, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/entrar", http.StatusSeeOther)
}

func (sv *Servidor) cambiarClaveWeb(w http.ResponseWriter, r *http.Request, s *Sesion) {
	var hash string
	sv.b.QueryRow(`SELECT hash FROM usuarios WHERE id=?`, s.Usuario.ID).Scan(&hash)
	p := Pagina{Titulo: "Contraseña", S: s, D: s.Usuario.DebeCambiar}
	switch {
	case !verificarClave(hash, r.PostFormValue("actual")):
		p.Err = "La contraseña actual no es correcta."
	case r.PostFormValue("nueva") != r.PostFormValue("repetir"):
		p.Err = "Las dos contraseñas nuevas no coinciden."
	case r.PostFormValue("nueva") == r.PostFormValue("actual"):
		p.Err = "La nueva contraseña debe ser distinta de la actual."
	default:
		if err := sv.b.cambiarClave(s.Usuario.ID, r.PostFormValue("nueva"), false, s.Usuario.Login, "WEB"); err != nil {
			p.Err = err.Error()
		} else {
			// cambiarClave cierra todas las sesiones: se abre una nueva.
			tok, _ := sv.b.crearSesion(s.Usuario.ID, sv.ip(r))
			http.SetCookie(w, &http.Cookie{Name: nombreCookie, Value: tok, Path: "/", HttpOnly: true,
				Secure: sv.cookieSegura || r.TLS != nil, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	}
	sv.render(w, "clave", p)
}

// ---------------------------------------------------------------------------
// Inicio, búsqueda y ficha
// ---------------------------------------------------------------------------

func (sv *Servidor) inicio(w http.ResponseWriter, r *http.Request, s *Sesion) {
	d := struct {
		Total, Vencidas int
		Version         int64
		Protocolo       string
		Resumen         *ResumenBD
	}{Version: sv.b.verReglas(), Protocolo: protocoloVersion}
	if puede(s, "pendientes") {
		ts, err := sv.b.pendientes(servicioDeRol(s.Usuario.Rol), hoy())
		if err == nil {
			d.Total = len(ts)
			for _, t := range ts {
				if t.Vencida {
					d.Vencidas++
				}
			}
		}
	}
	if s.Usuario.Rol == "ADMIN" || s.Usuario.Rol == "COORDINACION" {
		d.Resumen, _ = sv.b.resumen()
	}
	sv.render(w, "inicio", Pagina{Titulo: "Inicio", S: s, D: d})
}

func (sv *Servidor) buscar(w http.ResponseWriter, r *http.Request, s *Sesion) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	d := struct {
		Q  string
		ID int64
	}{Q: q}
	if q != "" {
		sv.aud(r, s, "BUSQUEDA", q, "")
		if id, err := sv.b.buscarMujer(q); err == nil {
			http.Redirect(w, r, fmt.Sprintf("/mujer/%d", id), http.StatusSeeOther)
			return
		}
	}
	sv.render(w, "buscar", Pagina{Titulo: "Buscar", S: s, D: d})
}

func idRuta(r *http.Request) int64 {
	n, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return n
}

var catCito = []string{"NEGATIVA", "ASCUS", "LSIL", "ASCH", "HSIL", "AGC", "AIS", "CARCINOMA", "INSATISFACTORIA"}
var catVPH = []string{"NEGATIVO", "POS_1618", "POS_OTROS", "NO_VALIDO"}
var catHisto = []string{"SIN_BIOPSIA", "NO_LESION", "LSIL_CIN1", "HSIL_CIN2", "HSIL_CIN3", "AIS", "CARCINOMA_ESCAMOSO", "ADENOCARCINOMA", "OTRO_INVASIVO", "INSATISFACTORIA"}
var motivosExcl = []string{"HISTERECTOMIA", "CANCER_CERVIX", "EMBARAZO", "PRUEBA_RECIENTE", "RECHAZO", "OTRO"}

func (sv *Servidor) mostrarFicha(w http.ResponseWriter, r *http.Request, s *Sesion, id int64, msg, errMsg string) {
	f, err := sv.b.cargarFicha(id)
	if err != nil {
		sv.fallo(w, s, http.StatusNotFound, "Mujer no encontrada")
		return
	}
	sv.aud(r, s, "ACCESO_FICHA", f.CIP, fmt.Sprintf("id %d", id))
	d := struct {
		F                                      *FichaMujer
		Edad                                   int
		Abierta                                *InvF
		ColPend                                *ColF
		CatCito, CatVPH, CatHisto, MotivosExcl []string
		Hoy                                    string
	}{F: f, Edad: edad(mustFecha(f.FechaNac), hoy()), Abierta: f.InvAbierta(),
		CatCito: catCito, CatVPH: catVPH, CatHisto: catHisto, MotivosExcl: motivosExcl, Hoy: fs(hoy())}
	if d.Abierta != nil {
		for i := range d.Abierta.Colpos {
			c := &d.Abierta.Colpos[i]
			if c.Estado == "DERIVADA" || (c.Estado == "REALIZADA" && c.Histologia == "") {
				d.ColPend = c
			}
		}
	}
	sv.render(w, "ficha", Pagina{Titulo: f.CIP, S: s, D: d, Msg: msg, Err: errMsg})
}

func (sv *Servidor) ficha(w http.ResponseWriter, r *http.Request, s *Sesion) {
	sv.mostrarFicha(w, r, s, idRuta(r), "", "")
}

// invDeMujer comprueba que la invitación del formulario es de esa mujer.
func (sv *Servidor) invDeMujer(x consultor, r *http.Request, mujerID int64) (int64, error) {
	inv, err := strconv.ParseInt(r.PostFormValue("inv"), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invitación no indicada")
	}
	var m int64
	if err := x.QueryRow(`SELECT mujer_id FROM invitaciones WHERE id=?`, inv).Scan(&m); err != nil || m != mujerID {
		return 0, fmt.Errorf("la invitación %d no pertenece a esta mujer", inv)
	}
	return inv, nil
}

// enTx ejecuta una acción clínica en una transacción y vuelve a la ficha.
func (sv *Servidor) enTx(w http.ResponseWriter, r *http.Request, s *Sesion, accion, okMsg string, f func(tx *Tx, pg Programa) (string, error)) {
	id := idRuta(r)
	pg := sv.pg(s)
	tx, err := sv.b.Begin()
	if err != nil {
		sv.mostrarFicha(w, r, s, id, "", err.Error())
		return
	}
	defer tx.Rollback()
	det, err := f(tx, pg)
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		// SQLite tiene una sola conexión: liberarla antes de auditar o leer
		// la ficha a través del pool. El rollback diferido queda de respaldo.
		tx.Rollback()
		sv.aud(r, s, accion+"_ERROR", fmt.Sprint(id), err.Error())
		sv.mostrarFicha(w, r, s, id, "", err.Error())
		return
	}
	sv.aud(r, s, accion, fmt.Sprint(id), det)
	sv.mostrarFicha(w, r, s, id, okMsg, "")
}

func fechaForm(r *http.Request, campo string, obligatoria bool) (time.Time, error) {
	v := strings.TrimSpace(r.PostFormValue(campo))
	if v == "" {
		if obligatoria {
			return time.Time{}, fmt.Errorf("falta la fecha %s", campo)
		}
		return time.Time{}, nil
	}
	t, err := parseFecha(v)
	if err == nil && t.After(hoy()) {
		return t, fmt.Errorf("la fecha %s no puede ser futura", campo)
	}
	return t, err
}

func (sv *Servidor) accionPrueba(w http.ResponseWriter, r *http.Request, s *Sesion) {
	sv.enTx(w, r, s, "REGISTRO_PRUEBA", "Prueba registrada.", func(tx *Tx, pg Programa) (string, error) {
		inv, err := sv.invDeMujer(tx, r, idRuta(r))
		if err != nil {
			return "", err
		}
		ft, err := fechaForm(r, "toma", true)
		if err != nil {
			return "", err
		}
		fr, err := fechaForm(r, "fres", false)
		if err != nil {
			return "", err
		}
		if !fr.IsZero() && fr.Before(ft) {
			return "", fmt.Errorf("la fecha de resultado es anterior a la toma")
		}
		d, err := pg.registrarPrueba(tx, inv, r.PostFormValue("tipo"), r.PostFormValue("finalidad"), r.PostFormValue("muestra"),
			ft, fr, r.PostFormValue("resultado"), "WEB")
		return fmt.Sprintf("inv %d %s %s -> %s", inv, r.PostFormValue("tipo"), r.PostFormValue("resultado"), d.Accion), err
	})
}

func (sv *Servidor) accionColposcopia(w http.ResponseWriter, r *http.Request, s *Sesion) {
	sv.enTx(w, r, s, "REGISTRO_COLPOSCOPIA", "Colposcopia registrada.", func(tx *Tx, pg Programa) (string, error) {
		inv, err := sv.invDeMujer(tx, r, idRuta(r))
		if err != nil {
			return "", err
		}
		fc, err := fechaForm(r, "fecha", true)
		if err != nil {
			return "", err
		}
		fh, err := fechaForm(r, "fhist", false)
		if err != nil {
			return "", err
		}
		h := r.PostFormValue("histologia")
		err = pg.registrarColposcopia(tx, inv, r.PostFormValue("estado"), fc, r.PostFormValue("adecuada") == "S",
			r.PostFormValue("biopsia") == "S", h, fh, "WEB")
		return fmt.Sprintf("inv %d %s %s", inv, r.PostFormValue("estado"), h), err
	})
}

func (sv *Servidor) accionExclusion(w http.ResponseWriter, r *http.Request, s *Sesion) {
	sv.enTx(w, r, s, "EXCLUSION", "Exclusión registrada.", func(tx *Tx, pg Programa) (string, error) {
		ini, err := parseFecha(r.PostFormValue("inicio"))
		if err != nil {
			return "", err
		}
		t, m := r.PostFormValue("tipo"), r.PostFormValue("motivo")
		var fin time.Time
		// Fin solo aplica a TEMPORAL; una definitiva ignora incluso un valor malformado.
		if t == "TEMPORAL" {
			if v := r.PostFormValue("fin"); v != "" {
				if fin, err = parseFecha(v); err != nil {
					return "", err
				}
			}
		}
		return t + " " + m, pg.crearExclusion(tx, idRuta(r), t, m, ini, fin, "WEB")
	})
}

func (sv *Servidor) accionNoValida(w http.ResponseWriter, r *http.Request, s *Sesion) {
	sv.enTx(w, r, s, "INVITACION_NO_VALIDA", "Invitación marcada como no válida.", func(tx *Tx, pg Programa) (string, error) {
		inv, err := sv.invDeMujer(tx, r, idRuta(r))
		if err != nil {
			return "", err
		}
		f, err := fechaForm(r, "fecha", true)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("inv %d %s", inv, r.PostFormValue("causa")), pg.marcarNoValida(tx, inv, r.PostFormValue("causa"), f, "WEB")
	})
}

func (sv *Servidor) accionReincorporar(w http.ResponseWriter, r *http.Request, s *Sesion) {
	id := idRuta(r)
	f, err := parseFecha(r.PostFormValue("fecha"))
	var cip string
	sv.b.QueryRow(`SELECT cip FROM mujeres WHERE id=?`, id).Scan(&cip)
	if err == nil {
		err = sv.pg(s).reincorporar(cip, f, r.PostFormValue("motivo"))
	}
	if err != nil {
		sv.mostrarFicha(w, r, s, id, "", err.Error())
		return
	}
	sv.aud(r, s, "REINCORPORACION", cip, r.PostFormValue("motivo"))
	sv.mostrarFicha(w, r, s, id, "Reincorporada al cribado.", "")
}

func (sv *Servidor) accionFusionar(w http.ResponseWriter, r *http.Request, s *Sesion) {
	id := idRuta(r)
	var cip string
	sv.b.QueryRow(`SELECT cip FROM mujeres WHERE id=?`, id).Scan(&cip)
	pg := sv.pg(s)
	det, err := pg.fusionar(cip, strings.TrimSpace(r.PostFormValue("duplicado")), r.PostFormValue("motivo"))
	if err != nil {
		sv.mostrarFicha(w, r, s, id, "", err.Error())
		return
	}
	sv.mostrarFicha(w, r, s, id, "Registros fusionados: "+det, "")
}

// ---------------------------------------------------------------------------
// Lista viva, KPIs, lotes, importación
// ---------------------------------------------------------------------------

func (sv *Servidor) pendientesWeb(w http.ResponseWriter, r *http.Request, s *Sesion) {
	srv := servicioDeRol(s.Usuario.Rol)
	elegir := srv == ""
	if elegir {
		srv = r.URL.Query().Get("servicio")
	}
	ts, err := sv.b.pendientes(srv, hoy())
	if err != nil {
		sv.fallo(w, s, 500, err.Error())
		return
	}
	venc := 0
	for _, t := range ts {
		if t.Vencida {
			venc++
		}
	}
	sv.aud(r, s, "PENDIENTES_CONSULTA", srv, fmt.Sprintf("%d tareas", len(ts)))
	sv.render(w, "pendientes", Pagina{Titulo: "Pendientes", S: s, D: struct {
		Servicio    string
		Servicios   []string
		Tareas      []Tarea
		Vencidas    int
		PuedeElegir bool
	}{srv, servicios, ts, venc, elegir}})
}

func anioParam(r *http.Request) int {
	a, err := strconv.Atoi(r.URL.Query().Get("anio"))
	if err != nil || a < 2000 || a > 2100 {
		return hoy().Year() - 1
	}
	return a
}

func (sv *Servidor) kpisWeb(w http.ResponseWriter, r *http.Request, s *Sesion) {
	a := anioParam(r)
	inf, err := sv.b.calcularKPIs(a)
	if err != nil {
		sv.fallo(w, s, 500, err.Error())
		return
	}
	sv.aud(r, s, "KPIS_CONSULTA", fmt.Sprint(a), "")
	sv.render(w, "kpis", Pagina{Titulo: "KPIs", S: s, D: struct {
		Anio int
		Inf  *Informe
	}{a, inf}})
}

func (sv *Servidor) kpisCSV(w http.ResponseWriter, r *http.Request, s *Sesion) {
	a := anioParam(r)
	inf, err := sv.b.calcularKPIs(a)
	if err != nil {
		sv.fallo(w, s, 500, err.Error())
		return
	}
	sv.aud(r, s, "KPIS_DESCARGA", fmt.Sprint(a), "")
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="kpis_pdpcx_%d.csv"`, a))
	cw := csv.NewWriter(w)
	cw.Comma = ';'
	cw.Write([]string{"anio", "hoja", "celda", "variable", "valor"})
	for _, c := range inf.Celdas {
		v := ""
		if c.Valor != nil {
			v = fmt.Sprint(*c.Valor)
		}
		cw.Write([]string{fmt.Sprint(a), c.Hoja, c.Celda, c.Desc, v})
	}
	cw.Flush()
}

type LoteF struct{ Lote, Fecha, Usuario, Detalle, Filas string }

func (sv *Servidor) lotes(w http.ResponseWriter, r *http.Request, s *Sesion) {
	sv.mostrarLotes(w, s, "", "")
}

func (sv *Servidor) mostrarLotes(w http.ResponseWriter, s *Sesion, msg, errMsg string) {
	rows, err := sv.b.Query(`SELECT COALESCE(fichero,''), fecha, COALESCE(usuario,''), COALESCE(detalle,''), COALESCE(filas,0)
	                         FROM cargas WHERE tipo='PLANIFICACION' ORDER BY id DESC LIMIT 200`)
	if err != nil {
		sv.fallo(w, s, 500, err.Error())
		return
	}
	defer rows.Close()
	var ls []LoteF
	for rows.Next() {
		var l LoteF
		var n int
		rows.Scan(&l.Lote, &l.Fecha, &l.Usuario, &l.Detalle, &n)
		l.Filas = fmt.Sprint(n)
		ls = append(ls, l)
	}
	sv.render(w, "lotes", Pagina{Titulo: "Invitaciones", S: s, D: ls, Msg: msg, Err: errMsg})
}

func (sv *Servidor) planificarWeb(w http.ResponseWriter, r *http.Request, s *Sesion) {
	d, e1 := parseFecha(r.PostFormValue("desde"))
	h, e2 := parseFecha(r.PostFormValue("hasta"))
	lote := strings.TrimSpace(r.PostFormValue("lote"))
	switch {
	case e1 != nil || e2 != nil || h.Before(d):
		sv.mostrarLotes(w, s, "", "Fechas no válidas")
		return
	case lote == "" || strings.ContainsAny(lote, " /\\;\"'"):
		sv.mostrarLotes(w, s, "", "Nombre de lote no válido")
		return
	}
	var n int
	sv.b.QueryRow(`SELECT COUNT(*) FROM invitaciones WHERE lote=?`, lote).Scan(&n)
	if n > 0 {
		sv.mostrarLotes(w, s, "", "Ya existe un lote con ese nombre")
		return
	}
	rs, err := sv.pg(s).planificar(d, h, lote, "WEB")
	if err != nil {
		sv.mostrarLotes(w, s, "", err.Error())
		return
	}
	tot := 0
	for _, v := range rs.Invitaciones {
		tot += v
	}
	sv.aud(r, s, "LOTE_PLANIFICADO", lote, fmt.Sprintf("%s..%s %d invitaciones", fs(d), fs(h), tot))
	sv.mostrarLotes(w, s, fmt.Sprintf("Lote %s: %d invitaciones. Cerradas sin respuesta: %d. Salen por edad: %d.", lote, tot, rs.CerradasSinRespuesta, rs.SalenPorEdad), "")
}

func (sv *Servidor) exportarWeb(w http.ResponseWriter, r *http.Request, s *Sesion) {
	lote := r.URL.Query().Get("lote")
	pg := sv.pg(s)
	filas, err := pg.filasLote(lote)
	if err != nil {
		sv.fallo(w, s, 500, err.Error())
		return
	}
	nombre := "lote_" + strings.Map(func(c rune) rune {
		if c == '-' || c == '_' || (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
			return c
		}
		return '_'
	}, lote) + ".csv"
	if err := pg.registrarExportacion(lote, "descarga web "+nombre, sv.ip(r), len(filas)); err != nil {
		sv.fallo(w, s, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+nombre+`"`)
	cw := csv.NewWriter(w)
	cw.Comma = ';'
	cw.Write(cabLote)
	cw.WriteAll(filas)
}

var tiposImport = []string{"PADRON", "PRUEBAS", "COLPOSCOPIAS", "NO_VALIDAS", "EXCLUSIONES", "BAJAS", "VACUNACION"}

func (sv *Servidor) importarForm(w http.ResponseWriter, r *http.Request, s *Sesion) {
	sv.render(w, "importar", Pagina{Titulo: "Importar", S: s, D: sv.datosImport(s, nil, nil)})
}

type DatosImport struct {
	Tipos    []string
	Res      *resumenImport
	Rechazos [][]string
	Hoy      string
}

func (sv *Servidor) datosImport(s *Sesion, res *resumenImport, rech [][]string) DatosImport {
	tipos := tiposImport
	if s.Usuario.Rol == "LABORATORIO" {
		tipos = []string{"PRUEBAS"}
	}
	return DatosImport{tipos, res, rech, fs(hoy())}
}

func (sv *Servidor) importarWeb(w http.ResponseWriter, r *http.Request, s *Sesion) {
	tipo := r.PostFormValue("tipo")
	permitido := false
	for _, t := range sv.datosImport(s, nil, nil).Tipos {
		permitido = permitido || t == tipo
	}
	p := Pagina{Titulo: "Importar", S: s}
	if !permitido {
		sv.aud(r, s, "ACCESO_DENEGADO", "/importar", "tipo "+tipo)
		sv.fallo(w, s, http.StatusForbidden, "No puede importar ese tipo de fichero.")
		return
	}
	fic, cab, err := r.FormFile("fichero")
	if err != nil {
		p.Err, p.D = "Falta el fichero", sv.datosImport(s, nil, nil)
		sv.render(w, "importar", p)
		return
	}
	defer fic.Close()
	dir, err := os.MkdirTemp("", "pdpcx-import-")
	if err != nil {
		sv.fallo(w, s, 500, err.Error())
		return
	}
	defer os.RemoveAll(dir)
	base := filepath.Base(cab.Filename)
	ruta := filepath.Join(dir, base)
	out, err := os.Create(ruta)
	if err != nil {
		sv.fallo(w, s, 500, err.Error())
		return
	}
	io.Copy(out, io.LimitReader(fic, 50<<20))
	out.Close()

	pg := sv.pg(s)
	var rs resumenImport
	switch tipo {
	case "PADRON":
		f := hoy()
		if v := r.PostFormValue("fecha"); v != "" {
			if t, e := parseFecha(v); e == nil {
				f = t
			}
		}
		var rc resumenCarga
		rc, err = pg.cargarPadron(ruta, f, "WEB:"+base)
		rs = resumenImport{Leidas: rc.Nuevas + rc.Actualizadas + rc.Errores + rc.FueraEdad, Aplicadas: rc.Nuevas + rc.Actualizadas, Rechazadas: rc.Errores}
		if err == nil {
			sv.aud(r, s, "IMPORTACION_PADRON", base, fmt.Sprintf("%+v", rc))
		}
	case "PRUEBAS":
		rs, err = pg.importarPruebas(ruta)
	case "COLPOSCOPIAS":
		rs, err = pg.importarColposcopias(ruta)
	case "NO_VALIDAS":
		rs, err = pg.importarNoValidas(ruta)
	case "EXCLUSIONES":
		rs, err = pg.importarExclusiones(ruta)
	case "BAJAS":
		rs, err = pg.importarBajas(ruta)
	case "VACUNACION":
		rs, err = pg.importarVacunacion(ruta)
	}
	if err != nil {
		p.Err, p.D = err.Error(), sv.datosImport(s, nil, nil)
		sv.render(w, "importar", p)
		return
	}
	var rech [][]string
	if rs.Rechazos != "" {
		if fr, e := leerCSVCrudo(rs.Rechazos); e == nil {
			rech = fr
		}
	}
	p.Msg, p.D = "Importación terminada.", sv.datosImport(s, &rs, rech)
	sv.render(w, "importar", p)
}

func leerCSVCrudo(ruta string) ([][]string, error) {
	f, err := os.Open(ruta)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cr := csv.NewReader(f)
	cr.Comma = ';'
	all, err := cr.ReadAll()
	if err != nil || len(all) == 0 {
		return nil, err
	}
	return all[1:], nil
}

// ---------------------------------------------------------------------------
// Zonas básicas de salud
// ---------------------------------------------------------------------------

type DatosZonas struct {
	Zonas, SinCatalogar []Zona
	SinZona             int
	F                   Zona // valores del formulario (zona en edición o vacío)
	Editando            bool
	Res                 *resumenImport
	Rechazos            [][]string
}

func (sv *Servidor) mostrarZonas(w http.ResponseWriter, s *Sesion, d DatosZonas, msg, errMsg string) {
	var err error
	if d.Zonas, d.SinCatalogar, d.SinZona, err = sv.b.listarZonas(); err != nil {
		sv.fallo(w, s, 500, err.Error())
		return
	}
	sv.render(w, "zonas", Pagina{Titulo: "Zonas", S: s, D: d, Msg: msg, Err: errMsg})
}

func (sv *Servidor) zonasWeb(w http.ResponseWriter, r *http.Request, s *Sesion) {
	d := DatosZonas{F: Zona{Activa: true}}
	if c := r.URL.Query().Get("editar"); c != "" {
		z, err := sv.b.zonaPorCodigo(c)
		if err != nil {
			sv.mostrarZonas(w, s, d, "", "No existe la zona "+normZBS(c))
			return
		}
		d.F, d.Editando = *z, true
	}
	sv.mostrarZonas(w, s, d, "", "")
}

func (sv *Servidor) zonaGuardar(w http.ResponseWriter, r *http.Request, s *Sesion) {
	f := r.PostFormValue
	z, err := zonaDeCampos(f("codigo"), f("nombre"), f("area"), f("centro"), f("municipio"), f("cupo_mensual"), f("activa"), f("notas"))
	if err == nil {
		err = sv.pg(s).guardarZona(sv.b, z)
	}
	if err != nil {
		sv.mostrarZonas(w, s, DatosZonas{F: z, Editando: f("editando") == "1"}, "", err.Error())
		return
	}
	cupo := "sin tope"
	if z.Cupo.Valid {
		cupo = fmt.Sprint(z.Cupo.Int64)
	}
	sv.aud(r, s, "ZBS_GUARDADA", z.Codigo, fmt.Sprintf("%s · cupo %s · activa %v", z.Nombre, cupo, z.Activa))
	sv.mostrarZonas(w, s, DatosZonas{F: Zona{Activa: true}}, "Zona "+z.Codigo+" guardada.", "")
}

func (sv *Servidor) zonasImportar(w http.ResponseWriter, r *http.Request, s *Sesion) {
	d := DatosZonas{F: Zona{Activa: true}}
	fic, cab, err := r.FormFile("fichero")
	if err != nil {
		sv.mostrarZonas(w, s, d, "", "Falta el fichero")
		return
	}
	defer fic.Close()
	dir, err := os.MkdirTemp("", "pdpcx-zbs-")
	if err != nil {
		sv.fallo(w, s, 500, err.Error())
		return
	}
	defer os.RemoveAll(dir)
	ruta := filepath.Join(dir, filepath.Base(cab.Filename))
	out, err := os.Create(ruta)
	if err == nil {
		_, err = io.Copy(out, fic)
		if e := out.Close(); err == nil {
			err = e
		}
	}
	if err != nil {
		sv.fallo(w, s, 500, err.Error())
		return
	}
	rs, err := sv.pg(s).importarZonas(ruta)
	if err != nil {
		sv.mostrarZonas(w, s, d, "", err.Error())
		return
	}
	d.Res = &rs
	if rs.Rechazos != "" {
		d.Rechazos, _ = leerCSVCrudo(rs.Rechazos)
	}
	sv.mostrarZonas(w, s, d, "Importación terminada.", "")
}

func (sv *Servidor) zonasPadron(w http.ResponseWriter, r *http.Request, s *Sesion) {
	d := DatosZonas{F: Zona{Activa: true}}
	n, err := sv.pg(s).altaZonasDelPadron()
	if err != nil {
		sv.mostrarZonas(w, s, d, "", err.Error())
		return
	}
	sv.aud(r, s, "ZBS_ALTA_PADRON", "", fmt.Sprintf("%d zonas", n))
	sv.mostrarZonas(w, s, d, fmt.Sprintf("%d zonas del padrón añadidas al catálogo. Complete su nombre y su cupo.", n), "")
}

func (sv *Servidor) zonasCSV(w http.ResponseWriter, r *http.Request, s *Sesion) {
	zs, _, _, err := sv.b.listarZonas()
	if err != nil {
		sv.fallo(w, s, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="zbs.csv"`)
	cw := csv.NewWriter(w)
	cw.Comma = ';'
	cw.Write(cabZonas)
	cw.WriteAll(filasZonas(zs))
}

// ---------------------------------------------------------------------------
// Parámetros, usuarios, auditoría
// ---------------------------------------------------------------------------

func (sv *Servidor) parametrosWeb(w http.ResponseWriter, r *http.Request, s *Sesion) {
	sv.mostrarParametros(w, s, "", "")
}

func (sv *Servidor) mostrarParametros(w http.ResponseWriter, s *Sesion, msg, errMsg string) {
	type ParF struct{ Clave, Valor, Desc string }
	var ps []ParF
	rows, err := sv.b.Query(`SELECT clave, valor, COALESCE(descripcion,'') FROM parametros ORDER BY clave`)
	if err != nil {
		sv.fallo(w, s, 500, err.Error())
		return
	}
	for rows.Next() {
		var p ParF
		rows.Scan(&p.Clave, &p.Valor, &p.Desc)
		ps = append(ps, p)
	}
	rows.Close()
	var vs [][4]string
	rows, err = sv.b.Query(`SELECT id, creada, usuario, motivo FROM reglas_versiones ORDER BY id DESC LIMIT 100`)
	if err == nil {
		for rows.Next() {
			var id int64
			var c, u, m string
			rows.Scan(&id, &c, &u, &m)
			vs = append(vs, [4]string{fmt.Sprint(id), c, u, m})
		}
		rows.Close()
	}
	sv.render(w, "parametros", Pagina{Titulo: "Reglas", S: s, Msg: msg, Err: errMsg, D: struct {
		Version   int64
		Protocolo string
		Pars      []ParF
		Versiones [][4]string
	}{sv.b.verReglas(), protocoloVersion, ps, vs}})
}

func (sv *Servidor) parametroCambiar(w http.ResponseWriter, r *http.Request, s *Sesion) {
	c, v, m := r.PostFormValue("clave"), strings.TrimSpace(r.PostFormValue("valor")), strings.TrimSpace(r.PostFormValue("motivo"))
	if err := sv.b.fijarParametro(c, v, s.Usuario.Login, "WEB", m); err != nil {
		sv.mostrarParametros(w, s, "", err.Error())
		return
	}
	sv.mostrarParametros(w, s, fmt.Sprintf("%s = %q · nueva versión v%d", c, v, sv.b.verReglas()), "")
}

func (sv *Servidor) usuariosWeb(w http.ResponseWriter, r *http.Request, s *Sesion) {
	sv.mostrarUsuarios(w, s, "", "")
}

func (sv *Servidor) mostrarUsuarios(w http.ResponseWriter, s *Sesion, msg, errMsg string) {
	us, err := sv.b.listarUsuarios()
	if err != nil {
		sv.fallo(w, s, 500, err.Error())
		return
	}
	sv.render(w, "usuarios", Pagina{Titulo: "Usuarios", S: s, Msg: msg, Err: errMsg, D: struct {
		Usuarios []Usuario
		Roles    []string
	}{us, roles}})
}

func (sv *Servidor) usuarioCrear(w http.ResponseWriter, r *http.Request, s *Sesion) {
	err := sv.b.crearUsuario(r.PostFormValue("login"), strings.TrimSpace(r.PostFormValue("nombre")), r.PostFormValue("rol"),
		r.PostFormValue("clave"), s.Usuario.Login, "WEB")
	if err != nil {
		sv.mostrarUsuarios(w, s, "", err.Error())
		return
	}
	sv.mostrarUsuarios(w, s, "Usuario creado.", "")
}

func (sv *Servidor) usuarioActivo(w http.ResponseWriter, r *http.Request, s *Sesion) {
	id := idRuta(r)
	if id == s.Usuario.ID {
		sv.mostrarUsuarios(w, s, "", "No puede desactivarse a sí mismo.")
		return
	}
	u, err := sv.b.usuarioPorID(id)
	if err != nil {
		sv.mostrarUsuarios(w, s, "", "Usuario no encontrado")
		return
	}
	nuevo, acc := 1, "USUARIO_ACTIVADO"
	if u.Activo {
		nuevo, acc = 0, "USUARIO_DESACTIVADO"
	}
	sv.b.Exec(`UPDATE usuarios SET activo=? WHERE id=?`, nuevo, id)
	if nuevo == 0 {
		sv.b.Exec(`DELETE FROM sesiones WHERE usuario_id=?`, id)
	}
	sv.aud(r, s, acc, u.Login, "")
	sv.mostrarUsuarios(w, s, acc+": "+u.Login, "")
}

func (sv *Servidor) usuarioClave(w http.ResponseWriter, r *http.Request, s *Sesion) {
	id := idRuta(r)
	if err := sv.b.cambiarClave(id, r.PostFormValue("clave"), true, s.Usuario.Login, "WEB"); err != nil {
		sv.mostrarUsuarios(w, s, "", err.Error())
		return
	}
	sv.mostrarUsuarios(w, s, "Contraseña restablecida: el usuario deberá cambiarla al entrar.", "")
}

func (sv *Servidor) auditoriaWeb(w http.ResponseWriter, r *http.Request, s *Sesion) {
	q := r.URL.Query()
	conds, args := []string{"1=1"}, []any{}
	if v := strings.TrimSpace(q.Get("usuario")); v != "" {
		conds, args = append(conds, "usuario=?"), append(args, strings.ToLower(v))
	}
	if v := strings.TrimSpace(q.Get("accion")); v != "" {
		conds, args = append(conds, "accion=?"), append(args, strings.ToUpper(v))
	}
	if v := strings.TrimSpace(q.Get("objeto")); v != "" {
		conds, args = append(conds, "objeto=?"), append(args, v)
	}
	if v := strings.TrimSpace(q.Get("desde")); v != "" {
		conds, args = append(conds, "ts>=?"), append(args, v)
	}
	rows, err := sv.b.Query(`SELECT ts, usuario, COALESCE(rol,''), canal, accion, COALESCE(objeto,''), COALESCE(detalle,''), COALESCE(ip,'')
	                         FROM auditoria WHERE `+strings.Join(conds, " AND ")+` ORDER BY id DESC LIMIT 500`, args...)
	if err != nil {
		sv.fallo(w, s, 500, err.Error())
		return
	}
	defer rows.Close()
	var filas [][8]string
	for rows.Next() {
		var f [8]string
		rows.Scan(&f[0], &f[1], &f[2], &f[3], &f[4], &f[5], &f[6], &f[7])
		filas = append(filas, f)
	}
	sv.render(w, "auditoria", Pagina{Titulo: "Auditoría", S: s, D: struct {
		Filas                          [][8]string
		Usuario, Accion, Objeto, Desde string
	}{filas, q.Get("usuario"), q.Get("accion"), q.Get("objeto"), q.Get("desde")}})
}

// ---------------------------------------------------------------------------

func servir(b *BD, dir, cert, key string, cookieSegura, confiarProxy bool) error {
	sv, err := nuevoServidor(b, cookieSegura || cert != "", confiarProxy)
	if err != nil {
		return err
	}
	var n int
	b.QueryRow(`SELECT COUNT(*) FROM usuarios WHERE rol='ADMIN' AND activo=1`).Scan(&n)
	if n == 0 {
		return fmt.Errorf("no hay ningún usuario ADMIN: créelo primero con 'pdpcx usuario-crear -rol ADMIN'")
	}
	srv := &http.Server{Addr: dir, Handler: sv.rutas(), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 5 * time.Minute, WriteTimeout: 5 * time.Minute, IdleTimeout: 2 * time.Minute}
	motor := "SQLite"
	if b.pg {
		motor = "PostgreSQL"
	}
	log.Printf("PDPCX web en %s (%s, reglas v%d). TLS: %v", dir, motor, b.verReglas(), cert != "")
	b.auditar("sistema", "", "WEB", "SERVIDOR_INICIO", dir, motor, "")
	if cert != "" {
		return srv.ListenAndServeTLS(cert, key)
	}
	return srv.ListenAndServe()
}
