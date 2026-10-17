package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

type sqliteTrialRoute struct{ target, path, heading string }
type sqliteTrialSample struct {
	seconds float64
	bytes   int
	err     error
}

// Validate the final destination as well as the body: following a redirect to
// the login form must never turn an unauthenticated request into a measurement.
func sqliteTrialValidate(status int, path, body string, route sqliteTrialRoute) error {
	if status != http.StatusOK || path != route.path ||
		!strings.Contains(body, route.heading) || !strings.Contains(body, "</html>") ||
		!strings.Contains(body, `aria-current="page"`) ||
		strings.Contains(body, `<h1>Acceso</h1>`) || strings.Contains(body, `<h1>Cambiar contraseña</h1>`) {
		return fmt.Errorf("unexpected response: status=%d path=%q wanted=%q heading=%q", status, path, route.path, route.heading)
	}
	return nil
}

func sqliteTrialRequest(c *http.Client, base string, route sqliteTrialRoute) sqliteTrialSample {
	start := time.Now()
	r, err := c.Get(base + route.target)
	if err != nil {
		return sqliteTrialSample{seconds: time.Since(start).Seconds(), err: err}
	}
	body, readErr := io.ReadAll(r.Body)
	closeErr := r.Body.Close()
	s := sqliteTrialSample{seconds: time.Since(start).Seconds(), bytes: len(body)}
	s.err = readErr
	if s.err == nil {
		s.err = closeErr
	}
	if s.err == nil {
		s.err = sqliteTrialValidate(r.StatusCode, r.Request.URL.Path, string(body), route)
	}
	return s
}

func sqliteTrialSeed(t *testing.T, b *BD, n int) {
	t.Helper()
	start := time.Now()
	tx, err := b.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	// ILLUSTRATIVE, not a clinical simulation. Each block of 100 has the
	// identical mix at both sizes. Ages span 20..69; due dates span 2026/2027.
	statements := []string{
		fmt.Sprintf(`WITH RECURSIVE seq(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM seq WHERE i<%d)
		INSERT INTO mujeres(id,cip,nhc,fecha_nacimiento,zbs,municipio,estado,fecha_estado,fecha_proxima,motivo_proxima,fecha_alta,origen)
		SELECT i,printf('TRIAL%%06d',i),printf('N%%06d',i),printf('%%04d-01-15',2026-(20+i%%50)),
		printf('Z%%02d',i%%10),'SINTETICO',
		CASE WHEN i%%100<10 THEN 'PENDIENTE_EDAD' WHEN i%%100<20 THEN 'SALIDA_EDAD'
		WHEN i%%100<25 THEN 'EXCLUIDA' WHEN i%%100<30 THEN 'BAJA'
		WHEN i%%100<40 THEN 'SEGUIMIENTO_CLINICO' ELSE 'ACTIVA' END,
		'2026-10-17',CASE WHEN i%%2=0 THEN '2026-09-01' ELSE '2027-04-01' END,
		'ILUSTRATIVO','2026-01-01','SQLITE_TRIAL' FROM seq`, n),
		`INSERT INTO invitaciones(id,mujer_id,tipo,ronda,fecha_invitacion,edad,prueba,muestra,motivo,estado,fecha_cierre,motivo_cierre,fecha_proxima)
		SELECT id,id,'CRIBADO',1,'2026-02-01',2026-CAST(substr(fecha_nacimiento,1,4) AS INTEGER),
		CASE WHEN id%50 BETWEEN 5 AND 9 THEN 'CITOLOGIA' ELSE 'VPH' END,'PROF','ILUSTRATIVO',
		CASE WHEN id%100<70 THEN 'CERRADA' WHEN id%100<80 THEN 'ENVIADA'
		WHEN id%100<90 THEN 'EN_CURSO' ELSE 'REPETIR_MUESTRA' END,
		CASE WHEN id%100<70 THEN '2026-03-01' END,
		CASE WHEN id%100<70 THEN 'RESULTADO' END,fecha_proxima FROM mujeres WHERE id%100>=40`,
		`INSERT INTO pruebas(invitacion_id,mujer_id,tipo,finalidad,muestra,fecha_toma,fecha_resultado,resultado,valida,origen)
		SELECT id,mujer_id,prueba,'PRIMARIA',muestra,'2026-02-15','2026-03-01',
		CASE WHEN prueba='CITOLOGIA' THEN 'NEGATIVA' ELSE 'NEGATIVO' END,1,'SQLITE_TRIAL'
		FROM invitaciones WHERE mujer_id%100<70`,
	}
	for _, q := range statements {
		if _, err := tx.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	t.Logf("TRIAL fixture n=%d seconds=%.6f label=ILLUSTRATIVE fixture_date=2026-10-17 handler_today=%s clock_frozen=false", n, time.Since(start).Seconds(), fs(hoy()))
	for _, check := range []struct {
		name, sql string
		want      int
	}{
		{"women", `SELECT COUNT(*) FROM mujeres`, n},
		{"invitations", `SELECT COUNT(*) FROM invitaciones`, n * 60 / 100},
		{"tests", `SELECT COUNT(*) FROM pruebas`, n * 30 / 100},
		{"closed", `SELECT COUNT(*) FROM invitaciones WHERE estado='CERRADA'`, n * 30 / 100},
		{"open", `SELECT COUNT(*) FROM invitaciones WHERE estado<>'CERRADA'`, n * 30 / 100},
		{"bad_link", `SELECT COUNT(*) FROM pruebas p LEFT JOIN invitaciones i ON i.id=p.invitacion_id WHERE i.id IS NULL OR i.mujer_id<>p.mujer_id`, 0},
	} {
		got := cuenta(t, b, check.sql)
		t.Logf("TRIAL count n=%d name=%s actual=%d expected=%d", n, check.name, got, check.want)
		if got != check.want {
			t.Fatalf("fixture %s: got %d want %d", check.name, got, check.want)
		}
	}
	for _, group := range []struct{ name, sql string }{
		{"state", `SELECT estado,COUNT(*) FROM mujeres GROUP BY estado ORDER BY estado`},
		{"age", `SELECT CAST(2026-CAST(substr(fecha_nacimiento,1,4) AS INTEGER) AS TEXT),COUNT(*) FROM mujeres GROUP BY 1 ORDER BY 1`},
		{"due", `SELECT fecha_proxima,COUNT(*) FROM mujeres GROUP BY fecha_proxima ORDER BY fecha_proxima`},
		{"invitation", `SELECT estado,COUNT(*) FROM invitaciones GROUP BY estado ORDER BY estado`},
		{"result", `SELECT tipo||':'||resultado,COUNT(*) FROM pruebas GROUP BY 1 ORDER BY 1`},
	} {
		rows, err := b.Query(group.sql)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var key string
			var count int
			if err := rows.Scan(&key, &count); err != nil {
				t.Fatal(err)
			}
			t.Logf("TRIAL distribution n=%d group=%s key=%q count=%d", n, group.name, key, count)
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("distribution: %v %v", err, closeErr)
		}
	}
	if count := cuenta(t, b, `SELECT COUNT(*) FROM pragma_foreign_key_check`); count != 0 {
		t.Fatalf("foreign key violations: %d", count)
	}
}

func sqliteTrialLogin(t *testing.T, b *BD, base string) *http.Client {
	t.Helper()
	if err := b.crearUsuario("trialadmin", "Synthetic trial admin", "ADMIN", "TrialInicial2026", "trial", "TEST"); err != nil {
		t.Fatal(err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Jar: jar, Timeout: 90 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	// Do not follow the password-change redirect to home: that would warm home
	// before its first sample. All auth requests and body reads are checked.
	consume := func(r *http.Response, err error, status int, path string) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(r.Body)
		closeErr := r.Body.Close()
		if readErr != nil || closeErr != nil || r.StatusCode != status || r.Request.URL.Path != path {
			t.Fatalf("auth response status=%d path=%s read=%v close=%v", r.StatusCode, r.Request.URL.Path, readErr, closeErr)
		}
		return string(body)
	}
	r, err := c.PostForm(base+"/entrar", url.Values{"login": {"trialadmin"}, "clave": {"TrialInicial2026"}})
	consume(r, err, http.StatusSeeOther, "/entrar")
	r, err = c.Get(base + "/clave")
	body := consume(r, err, http.StatusOK, "/clave")
	csrf := reCSRF.FindStringSubmatch(body)
	if csrf == nil || !strings.Contains(body, "Debe cambiar la contraseña") {
		t.Fatal("missing mandatory password change / CSRF")
	}
	r, err = c.PostForm(base+"/clave", url.Values{"csrf": {csrf[1]}, "actual": {"TrialInicial2026"}, "nueva": {"TrialNueva2027"}, "repetir": {"TrialNueva2027"}})
	consume(r, err, http.StatusSeeOther, "/clave")
	if r.Header.Get("Location") != "/" || cuenta(t, b, `SELECT COUNT(*) FROM usuarios WHERE login='trialadmin' AND debe_cambiar=0`) != 1 {
		t.Fatal("password change did not complete")
	}
	c.CheckRedirect = nil
	return c
}

// Opt-in diagnostic, not a functional regression or a claim about real history.
// "cold" means first measured route request, not a cold OS/SQLite cache.
func TestSQLiteTrial(t *testing.T) {
	if os.Getenv("PDPCX_SQLITE_TRIAL") != "1" {
		t.Skip("set PDPCX_SQLITE_TRIAL=1 for isolated synthetic measurement")
	}
	selected := os.Getenv("PDPCX_SQLITE_TRIAL_N")
	if selected != "" && selected != "10000" && selected != "100000" {
		t.Fatal("PDPCX_SQLITE_TRIAL_N must be 10000 or 100000")
	}
	routes := []sqliteTrialRoute{
		{"/", "/", "<h1>Inicio</h1>"},
		{"/pendientes", "/pendientes", "<h1>Pendientes"},
		{"/kpis?anio=2026", "/kpis", "<h1>KPIs 2026"},
		{"/buscar?q=TRIAL000060", "/mujer/60", "<h1>TRIAL000060"},
		{"/planificacion?anio=2027", "/planificacion", "<h1>Planificación mensual</h1>"},
	}
	for _, bad := range []struct{ status int; path, body string }{
		{200, "/entrar", "<h1>Acceso</h1></html>"},
		{404, "/missing", "not found"},
		{200, "/", "<h1>Inicio</h1>"},
	} {
		if sqliteTrialValidate(bad.status, bad.path, bad.body, routes[0]) == nil {
			t.Fatal("validator accepted invalid/missing/unauthenticated response")
		}
	}
	for _, n := range []int{10000, 100000} {
		if selected != "" && selected != fmt.Sprint(n) {
			continue
		}
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			b, _ := bdDe(t, motorTest{"sqlite", ""})
			sqliteTrialSeed(t, b, n)
			var journal string
			if err := b.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil || journal != "delete" || b.Stats().MaxOpenConnections != 1 {
				t.Fatalf("unexpected SQLite configuration: journal=%s pool=%d err=%v", journal, b.Stats().MaxOpenConnections, err)
			}
			sv, err := nuevoServidor(b, false, false)
			if err != nil {
				t.Fatal(err)
			}
			ts := httptest.NewServer(sv.rutas())
			defer ts.Close() // waits for handlers before the DB cleanup; go test bounds the process.
			c := sqliteTrialLogin(t, b, ts.URL)
			defer c.CloseIdleConnections()
			for _, route := range routes {
				var warm []float64
				bytes := -1
				for rep := 0; rep < 3; rep++ {
					s := sqliteTrialRequest(c, ts.URL, route)
					t.Logf("TRIAL sample n=%d route=%q repetition=%d cold=%t seconds=%.6f bytes=%d error=%q lower_bound=%t", n, route.target, rep, rep == 0, s.seconds, s.bytes, fmt.Sprint(s.err), s.err != nil)
					if s.err != nil {
						t.Fatalf("route failed after >=%.6fs; remaining samples unmeasured: %v", s.seconds, s.err)
					}
					if bytes >= 0 && bytes != s.bytes {
						t.Fatalf("response size changed: %d -> %d", bytes, s.bytes)
					}
					bytes = s.bytes
					if rep > 0 {
						warm = append(warm, s.seconds)
					}
				}
				sort.Float64s(warm)
				t.Logf("TRIAL warm n=%d route=%q samples=2 min=%.6f median=%.6f max=%.6f bytes=%d", n, route.target, warm[0], (warm[0]+warm[1])/2, warm[1], bytes)
			}
			// Optional shared authenticated client: pool/session contention included.
			if os.Getenv("PDPCX_SQLITE_TRIAL_CONCURRENT") == "1" {
				for _, route := range routes {
					results := make(chan sqliteTrialSample, 4)
					for i := 0; i < 4; i++ {
						go func() { results <- sqliteTrialRequest(c, ts.URL, route) }()
					}
					for i := 0; i < 4; i++ {
						s := <-results
						t.Logf("TRIAL concurrent n=%d route=%q seconds=%.6f bytes=%d error=%q lower_bound=%t", n, route.target, s.seconds, s.bytes, fmt.Sprint(s.err), s.err != nil)
						if s.err != nil {
							t.Errorf("concurrent route: %v", s.err)
						}
					}
				}
			}
		})
	}
}
