package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Los tests de integración se ejecutan en SQLite y, si se define
// PDPCX_TEST_PG (p. ej. postgres://pdpcx:pdpcx@127.0.0.1:5432/pdpcx?sslmode=disable),
// también en PostgreSQL, cada uno en un esquema propio que se borra al acabar.

func d(s string) time.Time { return mustFecha(s) }

type motorTest struct{ nombre, dsn string }

func motores(t *testing.T) []motorTest {
	ms := []motorTest{{"sqlite", ""}}
	if pg := os.Getenv("PDPCX_TEST_PG"); pg != "" {
		ms = append(ms, motorTest{"postgres", pg})
	}
	return ms
}

func bdDe(t *testing.T, m motorTest) (*BD, string) {
	t.Helper()
	dir := t.TempDir()
	if m.dsn == "" {
		b, err := abrirBD(filepath.Join(dir, "t.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { b.Close() })
		return b, dir
	}
	esquema := fmt.Sprintf("t_%d", time.Now().UnixNano())
	adm, err := sql.Open("pgx", m.dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adm.Exec("CREATE SCHEMA " + esquema); err != nil {
		t.Fatal(err)
	}
	sep := "?"
	if strings.Contains(m.dsn, "?") {
		sep = "&"
	}
	b, err := abrirBD(m.dsn + sep + "search_path=" + esquema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		b.Close()
		adm.Exec("DROP SCHEMA " + esquema + " CASCADE")
		adm.Close()
	})
	return b, dir
}

func enMotores(t *testing.T, f func(t *testing.T, b *BD, dir string)) {
	for _, m := range motores(t) {
		m := m
		t.Run(m.nombre, func(t *testing.T) {
			b, dir := bdDe(t, m)
			f(t, b, dir)
		})
	}
}

func cuenta(t *testing.T, b *BD, q string, args ...any) int {
	t.Helper()
	var n int
	if err := b.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(q, err)
	}
	return n
}

func pr(tipo, fin, res, f string) Prueba {
	return Prueba{Tipo: tipo, Finalidad: fin, Muestra: "PROF", FechaToma: d(f), FechaResultado: d(f).AddDate(0, 0, 10),
		Resultado: res, Valida: pruebaValida(tipo, res)}
}

func TestDecisionProtocolo(t *testing.T) {
	casos := []struct {
		nombre string
		ep     Episodio
		acc    string
	}{
		{"cito negativa -> rutina", Episodio{Tipo: "CRIBADO", Prueba: "CITOLOGIA", Pruebas: []Prueba{pr("CITOLOGIA", "PRIMARIA", "NEGATIVA", "2025-01-10")}}, accRutina},
		{"cito HSIL -> colposcopia", Episodio{Tipo: "CRIBADO", Prueba: "CITOLOGIA", Pruebas: []Prueba{pr("CITOLOGIA", "PRIMARIA", "HSIL", "2025-01-10")}}, accColposcopia},
		{"cito ASCUS sin triaje -> pedir VPH", Episodio{Tipo: "CRIBADO", Prueba: "CITOLOGIA", Pruebas: []Prueba{pr("CITOLOGIA", "PRIMARIA", "ASCUS", "2025-01-10")}}, accTriajeVPH},
		{"cito LSIL + VPH+ -> colposcopia", Episodio{Tipo: "CRIBADO", Prueba: "CITOLOGIA", Pruebas: []Prueba{
			pr("CITOLOGIA", "PRIMARIA", "LSIL", "2025-01-10"), pr("VPH", "TRIAJE", "POS_OTROS", "2025-01-10")}}, accColposcopia},
		{"cito ASCUS + VPH- -> control", Episodio{Tipo: "CRIBADO", Prueba: "CITOLOGIA", Pruebas: []Prueba{
			pr("CITOLOGIA", "PRIMARIA", "ASCUS", "2025-01-10"), pr("VPH", "TRIAJE", "NEGATIVO", "2025-01-10")}}, accControl},
		{"cito insatisfactoria -> repetir", Episodio{Tipo: "CRIBADO", Prueba: "CITOLOGIA", Pruebas: []Prueba{pr("CITOLOGIA", "PRIMARIA", "INSATISFACTORIA", "2025-01-10")}}, accRepetir},
		{"cito insatisfactoria x3 -> sin resultado", Episodio{Tipo: "CRIBADO", Prueba: "CITOLOGIA", Pruebas: []Prueba{
			pr("CITOLOGIA", "PRIMARIA", "INSATISFACTORIA", "2025-01-10"), pr("CITOLOGIA", "REPETICION", "INSATISFACTORIA", "2025-02-10"),
			pr("CITOLOGIA", "REPETICION", "INSATISFACTORIA", "2025-03-10")}}, accSinResultado},
		{"VPH negativo -> rutina", Episodio{Tipo: "CRIBADO", Prueba: "VPH", Pruebas: []Prueba{pr("VPH", "PRIMARIA", "NEGATIVO", "2025-01-10")}}, accRutina},
		{"VPH 16/18 -> colposcopia", Episodio{Tipo: "CRIBADO", Prueba: "VPH", Pruebas: []Prueba{pr("VPH", "PRIMARIA", "POS_1618", "2025-01-10")}}, accColposcopia},
		{"VPH otros sin triaje -> pedir citología", Episodio{Tipo: "CRIBADO", Prueba: "VPH", Pruebas: []Prueba{pr("VPH", "PRIMARIA", "POS_OTROS", "2025-01-10")}}, accTriajeCito},
		{"VPH otros + cito ASCUS -> colposcopia", Episodio{Tipo: "CRIBADO", Prueba: "VPH", Pruebas: []Prueba{
			pr("VPH", "PRIMARIA", "POS_OTROS", "2025-01-10"), pr("CITOLOGIA", "TRIAJE", "ASCUS", "2025-01-10")}}, accColposcopia},
		{"VPH otros + cito negativa -> control", Episodio{Tipo: "CRIBADO", Prueba: "VPH", Pruebas: []Prueba{
			pr("VPH", "PRIMARIA", "POS_OTROS", "2025-01-10"), pr("CITOLOGIA", "TRIAJE", "NEGATIVA", "2025-01-10")}}, accControl},
		{"VPH no válido -> repetir", Episodio{Tipo: "CRIBADO", Prueba: "VPH", Pruebas: []Prueba{pr("VPH", "PRIMARIA", "NO_VALIDO", "2025-01-10")}}, accRepetir},
		{"repetición válida tras no válido manda", Episodio{Tipo: "CRIBADO", Prueba: "VPH", Pruebas: []Prueba{
			pr("VPH", "PRIMARIA", "NO_VALIDO", "2025-01-10"), pr("VPH", "REPETICION", "NEGATIVO", "2025-02-10")}}, accRutina},
		{"control VPH persistente -> colposcopia", Episodio{Tipo: "CONTROL", Prueba: "VPH", Pruebas: []Prueba{pr("VPH", "CONTROL", "POS_OTROS", "2026-01-10")}}, accColposcopia},
		{"control VPH negativo -> rutina", Episodio{Tipo: "CONTROL", Prueba: "VPH", Pruebas: []Prueba{pr("VPH", "CONTROL", "NEGATIVO", "2026-01-10")}}, accRutina},
		{"control cito ASCUS -> colposcopia", Episodio{Tipo: "CONTROL", Prueba: "CITOLOGIA", Pruebas: []Prueba{pr("CITOLOGIA", "CONTROL", "ASCUS", "2026-01-10")}}, accColposcopia},
		{"sin pruebas -> esperar", Episodio{Tipo: "CRIBADO", Prueba: "VPH"}, accEsperar},
	}
	for _, c := range casos {
		if got := decidir(c.ep, 2); got.Accion != c.acc {
			t.Errorf("%s: obtenido %s (%s), esperado %s", c.nombre, got.Accion, got.Motivo, c.acc)
		}
	}
}

func TestRebindPostgres(t *testing.T) {
	b := &BD{pg: true}
	got := b.q(`SELECT ? FROM t WHERE a='¿?' AND b=? AND c IN (?,?)`)
	want := `SELECT $1 FROM t WHERE a='¿?' AND b=$2 AND c IN ($3,$4)`
	if got != want {
		t.Errorf("rebind:\n %s\n %s", got, want)
	}
}

func padronCSV(t *testing.T, dir string, filas [][]string) string {
	t.Helper()
	ruta := filepath.Join(dir, fmt.Sprintf("p%d.csv", time.Now().UnixNano()))
	if err := escribirCSV(ruta, []string{"cip", "nhc", "fecha_nacimiento", "zbs", "municipio", "cod_postal"}, filas); err != nil {
		t.Fatal(err)
	}
	return ruta
}

func TestLimitesDeEdad(t *testing.T) {
	enMotores(t, func(t *testing.T, b *BD, dir string) {
		pg := Programa{b, "test", "TEST"}
		// A: cumple 66 el 2025-06-15; cita el día anterior -> se invita con 65.
		// B: cumple 66 el 2025-03-01; cita después -> NO se invita, sale por edad.
		// C: cumple 25 el 2025-09-20 -> entra con el alta anual y se invita ese día.
		// D: cumple 30 el 2025-02-01, cita en abril -> VPH.
		csv := padronCSV(t, dir, [][]string{{"A", "1", "1959-06-15"}, {"B", "2", "1959-03-01"}, {"C", "3", "2000-09-20"}, {"D", "4", "1995-02-01"}})
		if _, err := pg.cargarPadron(csv, d("2025-01-01"), "TEST"); err != nil {
			t.Fatal(err)
		}
		b.Exec(`UPDATE mujeres SET fecha_proxima='2025-06-14' WHERE cip='A'`)
		b.Exec(`UPDATE mujeres SET fecha_proxima='2025-05-01' WHERE cip='B'`)
		b.Exec(`UPDATE mujeres SET fecha_proxima='2025-04-10' WHERE cip='D'`)
		if _, err := pg.altaAnual(2025, "TEST"); err != nil {
			t.Fatal(err)
		}
		if _, err := pg.planificar(d("2025-01-01"), d("2025-12-31"), "T", "TEST"); err != nil {
			t.Fatal(err)
		}
		inv := func(cip string) (string, int, string, bool) {
			var f, p string
			var e int
			err := b.QueryRow(`SELECT i.fecha_invitacion, i.edad, i.prueba FROM invitaciones i JOIN mujeres m ON m.id=i.mujer_id WHERE m.cip=?`, cip).Scan(&f, &e, &p)
			return f, e, p, err == nil
		}
		if f, e, p, ok := inv("A"); !ok || e != 65 || p != "VPH" || f != "2025-06-14" {
			t.Errorf("A: esperada VPH a los 65 el 2025-06-14; obtenido %v %s %d %s", ok, f, e, p)
		}
		if _, _, _, ok := inv("B"); ok {
			t.Errorf("B: no debía invitarse")
		}
		var est string
		b.QueryRow(`SELECT estado FROM mujeres WHERE cip='B'`).Scan(&est)
		if est != "SALIDA_EDAD" {
			t.Errorf("B: estado %s, esperado SALIDA_EDAD", est)
		}
		if f, e, p, ok := inv("C"); !ok || e != 25 || p != "CITOLOGIA" || f != "2025-09-20" {
			t.Errorf("C: esperada citología a los 25 el 2025-09-20; obtenido %v %s %d %s", ok, f, e, p)
		}
		if _, e, p, ok := inv("D"); !ok || e != 30 || p != "VPH" {
			t.Errorf("D: esperada VPH a los 30; obtenido %v %d %s", ok, e, p)
		}
	})
}

func TestVersionadoReglas(t *testing.T) {
	enMotores(t, func(t *testing.T, b *BD, dir string) {
		pg := Programa{b, "coord", "TEST"}
		v1 := b.verReglas()
		if err := b.fijarParametro("control_meses", "18", "coord", "TEST", ""); err == nil {
			t.Error("debía exigir motivo")
		}
		if err := b.fijarParametro("control_meses", "dieciocho", "coord", "TEST", "x"); err == nil {
			t.Error("debía rechazar un valor no numérico")
		}
		if err := b.fijarParametro("control_meses", "18", "coord", "TEST", "acuerdo comité 3/2027"); err != nil {
			t.Fatal(err)
		}
		v2 := b.verReglas()
		if v2 != v1+1 || b.pInt("control_meses") != 18 {
			t.Fatalf("versión %d->%d, control_meses=%d", v1, v2, b.pInt("control_meses"))
		}
		var foto string
		b.QueryRow(`SELECT parametros FROM reglas_versiones WHERE id=?`, v2).Scan(&foto)
		if !strings.Contains(foto, `"control_meses":"18"`) {
			t.Errorf("la foto de la versión no contiene el cambio: %s", foto)
		}
		if cuenta(t, b, `SELECT COUNT(*) FROM auditoria WHERE accion='PARAMETRO_CAMBIADO' AND objeto='control_meses'`) != 1 {
			t.Error("el cambio no quedó en auditoría")
		}
		// Una decisión tomada ahora queda con la versión v2 y aplica 18 meses.
		csv := padronCSV(t, dir, [][]string{{"V1", "1", "1980-05-05"}})
		pg.cargarPadron(csv, d("2025-01-01"), "TEST")
		b.Exec(`UPDATE mujeres SET fecha_proxima='2025-02-01'`)
		pg.planificar(d("2025-01-01"), d("2025-12-31"), "T", "TEST")
		var inv int64
		var vInv int64
		b.QueryRow(`SELECT id, version_reglas FROM invitaciones`).Scan(&inv, &vInv)
		if vInv != v2 {
			t.Errorf("la invitación guarda versión %d, esperada %d", vInv, v2)
		}
		tx, _ := b.Begin()
		pg.registrarPrueba(tx, inv, "VPH", "PRIMARIA", "PROF", d("2025-02-10"), d("2025-02-20"), "POS_OTROS", "TEST")
		pg.registrarPrueba(tx, inv, "CITOLOGIA", "TRIAJE", "PROF", d("2025-02-10"), d("2025-02-25"), "NEGATIVA", "TEST")
		tx.Commit()
		var acc string
		var vDec int64
		b.QueryRow(`SELECT accion, version_reglas FROM decisiones WHERE invitacion_id=? ORDER BY id DESC LIMIT 1`, inv).Scan(&acc, &vDec)
		if acc != accControl || vDec != v2 {
			t.Errorf("decisión %s v%d, esperada %s v%d", acc, vDec, accControl, v2)
		}
		var prox string
		b.QueryRow(`SELECT fecha_proxima FROM mujeres WHERE cip='V1'`).Scan(&prox)
		if prox != "2026-08-25" {
			t.Errorf("control a 18 meses: próxima %s, esperada 2026-08-25", prox)
		}
	})
}

func TestRegistroMaestro(t *testing.T) {
	enMotores(t, func(t *testing.T, b *BD, dir string) {
		pg := Programa{b, "admision1", "TEST"}
		csv := padronCSV(t, dir, [][]string{{"P1", "100", "1980-01-01", "ZBS01", "26001", "26001"}, {"D1", "200", "1980-01-01", "ZBS02", "26002", "26002"}})
		pg.cargarPadron(csv, d("2025-01-01"), "TEST")
		// Segunda carga con cambios de ZBS y NHC -> historial con valor anterior.
		csv2 := padronCSV(t, dir, [][]string{{"P1", "101", "1980-01-01", "ZBS07", "26001", "26001"}})
		if _, err := pg.cargarPadron(csv2, d("2025-03-01"), "TEST"); err != nil {
			t.Fatal(err)
		}
		var ant, nue string
		b.QueryRow(`SELECT anterior, nuevo FROM historial_persona h JOIN mujeres m ON m.id=h.mujer_id WHERE m.cip='P1' AND campo='zbs'`).Scan(&ant, &nue)
		if ant != "ZBS01" || nue != "ZBS07" {
			t.Errorf("historial zbs %q -> %q", ant, nue)
		}
		if cuenta(t, b, `SELECT COUNT(*) FROM historial_persona`) != 2 {
			t.Error("esperados 2 cambios (nhc y zbs)")
		}
		// Vacunación: idempotente.
		vac := filepath.Join(dir, "vac.csv")
		escribirCSV(vac, []string{"cip", "fecha", "dosis", "vacuna"}, [][]string{{"D1", "2010-03-01", "1", "VPH9"}, {"D1", "2010-03-01", "1", "VPH9"}})
		pg.importarVacunacion(vac)
		if cuenta(t, b, `SELECT COUNT(*) FROM vacunacion_vph`) != 1 {
			t.Error("la vacuna repetida se duplicó")
		}
		// Fusión: D1 tiene una invitación con prueba; pasa a P1.
		b.Exec(`UPDATE mujeres SET fecha_proxima='2025-04-01' WHERE cip='D1'`)
		b.Exec(`UPDATE mujeres SET fecha_proxima='2026-04-01' WHERE cip='P1'`)
		pg.planificar(d("2025-04-01"), d("2025-04-30"), "F", "TEST")
		var inv int64
		b.QueryRow(`SELECT i.id FROM invitaciones i JOIN mujeres m ON m.id=i.mujer_id WHERE m.cip='D1'`).Scan(&inv)
		tx, _ := b.Begin()
		pg.registrarPrueba(tx, inv, "VPH", "PRIMARIA", "PROF", d("2025-04-10"), d("2025-04-20"), "NEGATIVO", "TEST")
		tx.Commit()
		if _, err := pg.fusionar("P1", "D1", ""); err == nil {
			t.Error("la fusión debía exigir motivo")
		}
		det, err := pg.fusionar("P1", "D1", "mismo DNI")
		if err != nil {
			t.Fatal(err)
		}
		var est string
		var fus sql.NullInt64
		b.QueryRow(`SELECT estado, fusionada_en FROM mujeres WHERE cip='D1'`).Scan(&est, &fus)
		var idP int64
		b.QueryRow(`SELECT id FROM mujeres WHERE cip='P1'`).Scan(&idP)
		if est != "FUSIONADA" || fus.Int64 != idP {
			t.Errorf("duplicada: estado %s fusionada_en %v", est, fus)
		}
		if cuenta(t, b, `SELECT COUNT(*) FROM invitaciones WHERE mujer_id=?`, idP) != 1 || cuenta(t, b, `SELECT COUNT(*) FROM pruebas WHERE mujer_id=?`, idP) != 1 ||
			cuenta(t, b, `SELECT COUNT(*) FROM vacunacion_vph WHERE mujer_id=?`, idP) != 1 {
			t.Errorf("no se movió todo a la principal: %s", det)
		}
		if cuenta(t, b, `SELECT COUNT(*) FROM fusiones`) != 1 || cuenta(t, b, `SELECT COUNT(*) FROM auditoria WHERE accion='FUSION'`) != 1 {
			t.Error("la fusión no quedó registrada")
		}
		if _, err := pg.fusionar("P1", "D1", "otra vez"); err == nil {
			t.Error("no se debe poder fusionar dos veces")
		}
	})
}

func TestListaViva(t *testing.T) {
	enMotores(t, func(t *testing.T, b *BD, dir string) {
		pg := Programa{b, "test", "TEST"}
		b.fijarParametro("autotoma_inicio", "2025-01-01", "t", "TEST", "prueba")
		b.fijarParametro("autotoma_estrategia", "TODAS", "t", "TEST", "prueba")
		csv := padronCSV(t, dir, [][]string{{"SIN", "1", "1980-01-01"}, {"AUT", "2", "1980-01-01"}, {"LAB", "3", "1970-01-01"}, {"GIN", "4", "1975-01-01"}})
		pg.cargarPadron(csv, d("2025-01-01"), "TEST")
		b.Exec(`UPDATE mujeres SET fecha_proxima='2025-01-10'`)
		pg.planificar(d("2025-01-01"), d("2025-01-31"), "T", "TEST")
		ids := map[string]int64{}
		rows, _ := b.Query(`SELECT m.cip, i.id FROM invitaciones i JOIN mujeres m ON m.id=i.mujer_id`)
		for rows.Next() {
			var c string
			var i int64
			rows.Scan(&c, &i)
			ids[c] = i
		}
		rows.Close()
		b.Exec(`UPDATE invitaciones SET muestra='PROF' WHERE id IN (?,?)`, ids["LAB"], ids["GIN"])
		tx, _ := b.Begin()
		pg.registrarPrueba(tx, ids["AUT"], "VPH", "PRIMARIA", "AUTO", d("2025-01-20"), d("2025-02-01"), "POS_OTROS", "T")
		pg.registrarPrueba(tx, ids["LAB"], "VPH", "PRIMARIA", "PROF", d("2025-01-20"), d("2025-02-01"), "POS_OTROS", "T")
		pg.registrarPrueba(tx, ids["GIN"], "VPH", "PRIMARIA", "PROF", d("2025-01-20"), d("2025-02-01"), "POS_1618", "T")
		tx.Commit()
		ts, err := b.pendientes("", d("2025-04-15"))
		if err != nil {
			t.Fatal(err)
		}
		hay := func(serv, cip string, vencida bool) bool {
			for _, x := range ts {
				if x.Servicio == serv && x.CIP == cip && x.Vencida == vencida {
					return true
				}
			}
			return false
		}
		if !hay("ADMISION", "SIN", false) {
			t.Error("falta recordatorio a la que no responde")
		}
		if !hay("AP", "AUT", true) {
			t.Error("falta citar toma profesional tras autotoma VPH+ (vencida)")
		}
		if !hay("LABORATORIO", "LAB", true) {
			t.Error("falta triaje réflex pendiente (vencido)")
		}
		if !hay("GINECOLOGIA", "GIN", true) {
			t.Error("falta colposcopia pendiente fuera de plazo (73 días > 60)")
		}
		soloAP, _ := b.pendientes("AP", d("2025-04-15"))
		for _, x := range soloAP {
			if x.Servicio != "AP" {
				t.Errorf("AP ve una tarea de %s", x.Servicio)
			}
		}
	})
}

func TestSimulacionIntegridad(t *testing.T) {
	if testing.Short() {
		t.Skip("simulación completa")
	}
	enMotores(t, func(t *testing.T, b *BD, dir string) {
		b.fijarParametro("fecha_inicio_programa", "2024-01-01", "t", "TEST", "simulación")
		b.fijarParametro("autotoma_inicio", "2025-07-01", "t", "TEST", "simulación")
		csv := filepath.Join(dir, "p.csv")
		if err := generarPadron(csv, 2000, 2024, 7); err != nil {
			t.Fatal(err)
		}
		pg := Programa{b, "simulador", "SIMULADOR"}
		if _, err := pg.cargarPadron(csv, d("2024-01-01"), "TEST"); err != nil {
			t.Fatal(err)
		}
		devnull, _ := os.Open(os.DevNull)
		stdout := os.Stdout
		os.Stdout = devnull
		for a := 2024; a <= 2026; a++ {
			tc := 50
			if a == 2024 {
				tc = 0
			}
			if err := simularAnio(b, a, 7, tc, dir); err != nil {
				os.Stdout = stdout
				t.Fatal(err)
			}
		}
		os.Stdout = stdout

		reglas := map[string]string{
			"invitación fuera de 25-65":          `SELECT COUNT(*) FROM invitaciones WHERE edad < 25 OR edad > 65`,
			"prueba no acorde a la edad":         `SELECT COUNT(*) FROM invitaciones WHERE (edad <= 29 AND prueba<>'CITOLOGIA') OR (edad >= 30 AND prueba<>'VPH')`,
			"autotoma fuera de VPH":              `SELECT COUNT(*) FROM invitaciones WHERE muestra='AUTO' AND prueba<>'VPH'`,
			"autotoma antes de su inicio":        `SELECT COUNT(*) FROM invitaciones WHERE muestra='AUTO' AND fecha_invitacion < '2025-07-01'`,
			"más de una invitación abierta":      `SELECT COUNT(*) FROM (SELECT mujer_id FROM invitaciones WHERE estado<>'CERRADA' GROUP BY mujer_id HAVING COUNT(*)>1) x`,
			"toma anterior a la invitación":      `SELECT COUNT(*) FROM pruebas p JOIN invitaciones i ON i.id=p.invitacion_id WHERE p.fecha_toma < i.fecha_invitacion`,
			"resultado anterior a la toma":       `SELECT COUNT(*) FROM pruebas WHERE fecha_resultado < fecha_toma`,
			"invitación sin evento":              `SELECT COUNT(*) FROM invitaciones i WHERE NOT EXISTS (SELECT 1 FROM eventos e WHERE e.invitacion_id=i.id AND e.tipo='INVITACION')`,
			"prueba sin evento":                  `SELECT COUNT(*) FROM pruebas p WHERE NOT EXISTS (SELECT 1 FROM eventos e WHERE e.invitacion_id=p.invitacion_id AND e.tipo='PRUEBA_'||p.tipo)`,
			"evento sin usuario":                 `SELECT COUNT(*) FROM eventos WHERE usuario IS NULL OR usuario=''`,
			"invitación sin versión de reglas":   `SELECT COUNT(*) FROM invitaciones WHERE version_reglas IS NULL`,
			"episodio con pruebas sin decisión":  `SELECT COUNT(*) FROM invitaciones i WHERE EXISTS (SELECT 1 FROM pruebas p WHERE p.invitacion_id=i.id) AND NOT EXISTS (SELECT 1 FROM decisiones x WHERE x.invitacion_id=i.id)`,
			"CIN2+ que sigue ACTIVA":             `SELECT COUNT(*) FROM colposcopias c JOIN mujeres m ON m.id=c.mujer_id WHERE c.histologia IN ('HSIL_CIN2','HSIL_CIN3','AIS','CARCINOMA_ESCAMOSO','ADENOCARCINOMA','OTRO_INVASIVO') AND m.estado='ACTIVA'`,
			"control sin episodio de origen":     `SELECT COUNT(*) FROM invitaciones WHERE tipo='CONTROL' AND episodio_origen IS NULL`,
			"invitada tras exclusión definitiva": `SELECT COUNT(*) FROM invitaciones i JOIN exclusiones x ON x.mujer_id=i.mujer_id AND x.tipo='DEFINITIVA' AND i.fecha_invitacion > x.fecha_inicio`,
			"invitada en exclusión temporal":     `SELECT COUNT(*) FROM invitaciones i JOIN exclusiones x ON x.mujer_id=i.mujer_id AND x.tipo='TEMPORAL' AND i.fecha_invitacion BETWEEN x.fecha_inicio AND x.fecha_fin`,
		}
		for n, q := range reglas {
			if c := cuenta(t, b, q); c != 0 {
				t.Errorf("%s: %d casos", n, c)
			}
		}
		// Nadie invitado en o después de cumplir 66 (calculado en Go, portable).
		rows, _ := b.Query(`SELECT m.fecha_nacimiento, i.fecha_invitacion FROM invitaciones i JOIN mujeres m ON m.id=i.mujer_id`)
		for rows.Next() {
			var fn, fi string
			rows.Scan(&fn, &fi)
			if !mustFecha(fi).Before(cumple(mustFecha(fn), 66)) {
				t.Errorf("invitada con 66: nacida %s invitada %s", fn, fi)
			}
		}
		rows.Close()
		for _, q := range []string{
			`SELECT COUNT(*) FROM invitaciones WHERE muestra='AUTO'`,
			`SELECT COUNT(*) FROM invitaciones WHERE tipo='CONTROL'`,
			`SELECT COUNT(*) FROM mujeres WHERE cip LIKE 'C2025%' AND estado<>'PENDIENTE_EDAD'`,
		} {
			if cuenta(t, b, q) == 0 {
				t.Errorf("sin casos: %s", q)
			}
		}
		for a := 2024; a <= 2026; a++ {
			inf, err := b.calcularKPIs(a)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range inf.Controles {
				if !strings.HasPrefix(c[1], "OK") {
					t.Errorf("KPIs %d: %s -> %s", a, c[0], c[1])
				}
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Web
// ---------------------------------------------------------------------------

var reCSRF = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

type navegador struct {
	t   *testing.T
	c   *http.Client
	url string
}

func nuevoNavegador(t *testing.T, base string) *navegador {
	jar, _ := cookiejar.New(nil)
	return &navegador{t, &http.Client{Jar: jar}, base}
}

func (n *navegador) get(p string) (int, string) {
	r, err := n.c.Get(n.url + p)
	if err != nil {
		n.t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, string(b)
}

func (n *navegador) post(p string, v url.Values) (int, string) {
	r, err := n.c.PostForm(n.url+p, v)
	if err != nil {
		n.t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, string(b)
}

func (n *navegador) csrf() string {
	_, body := n.get("/clave")
	m := reCSRF.FindStringSubmatch(body)
	if m == nil {
		n.t.Fatal("sin token CSRF")
	}
	return m[1]
}

func (n *navegador) entrar(login, clave string) (int, string) {
	return n.post("/entrar", url.Values{"login": {login}, "clave": {clave}})
}

// entrarYCambiar: primer acceso, con cambio obligatorio de contraseña.
func (n *navegador) entrarYCambiar(login, inicial, nueva string) {
	if c, body := n.entrar(login, inicial); c != 200 || !strings.Contains(body, "Debe cambiar la contraseña") {
		n.t.Fatalf("%s: primer acceso debía pedir cambio de clave (código %d)", login, c)
	}
	c, body := n.post("/clave", url.Values{"csrf": {n.csrf()}, "actual": {inicial}, "nueva": {nueva}, "repetir": {nueva}})
	if c != 200 || !strings.Contains(body, "Inicio") {
		n.t.Fatalf("%s: cambio de clave falló (%d)", login, c)
	}
}

func TestWeb(t *testing.T) {
	enMotores(t, func(t *testing.T, b *BD, dir string) {
		pg := Programa{b, "test", "TEST"}
		csv := padronCSV(t, dir, [][]string{{"W1", "900", "1980-01-01"}})
		pg.cargarPadron(csv, d("2025-01-01"), "TEST")
		b.Exec(`UPDATE mujeres SET fecha_proxima='2025-01-10'`)
		pg.planificar(d("2025-01-01"), d("2025-01-31"), "LW", "TEST")
		var mid, inv int64
		b.QueryRow(`SELECT mujer_id, id FROM invitaciones`).Scan(&mid, &inv)

		if err := b.crearUsuario("admin", "Administración", "ADMIN", "corta", "t", "TEST"); err == nil {
			t.Error("debía rechazar una contraseña corta")
		}
		if err := b.crearUsuario("admin", "Administración", "ADMIN", "ClaveInicial2027", "t", "TEST"); err != nil {
			t.Fatal(err)
		}
		sv, err := nuevoServidor(b, false, false)
		if err != nil {
			t.Fatal(err)
		}
		ts := httptest.NewServer(sv.rutas())
		defer ts.Close()

		// Sin sesión -> a la pantalla de acceso; cabeceras de seguridad.
		an := nuevoNavegador(t, ts.URL)
		r, _ := an.c.Get(ts.URL + "/mujer/1")
		if r.Request.URL.Path != "/entrar" {
			t.Errorf("sin sesión debía redirigir a /entrar, fue a %s", r.Request.URL.Path)
		}
		if !strings.Contains(r.Header.Get("Content-Security-Policy"), "default-src 'self'") || r.Header.Get("X-Frame-Options") != "DENY" {
			t.Error("faltan cabeceras de seguridad")
		}
		r.Body.Close()
		if c, _ := an.entrar("admin", "mal"); c != 401 {
			t.Errorf("clave mala: código %d", c)
		}
		admin := nuevoNavegador(t, ts.URL)
		admin.entrarYCambiar("admin", "ClaveInicial2027", "NuevaClaveAdmin2027")
		u, _ := url.Parse(ts.URL)
		for _, ck := range admin.c.Jar.Cookies(u) {
			if ck.Name == nombreCookie && ck.Value == "" {
				t.Error("cookie de sesión vacía")
			}
		}
		// Crear usuarios por la web; sin CSRF se rechaza.
		if c, _ := admin.post("/usuarios", url.Values{"login": {"x"}, "nombre": {"X"}, "rol": {"AP"}, "clave": {"ClaveProvisional1"}}); c != 403 {
			t.Errorf("POST sin CSRF: código %d, esperado 403", c)
		}
		tok := admin.csrf()
		for _, x := range [][3]string{{"enfermera", "AP"}, {"gine", "GINECOLOGIA"}, {"consulta", "CONSULTA"}, {"coord", "COORDINACION"}} {
			if c, body := admin.post("/usuarios", url.Values{"csrf": {tok}, "login": {x[0]}, "nombre": {x[0]}, "rol": {x[1]}, "clave": {"ClaveProvisional1"}}); c != 200 || !strings.Contains(body, "Usuario creado") {
				t.Fatalf("crear %s: %d", x[0], c)
			}
		}
		// CONSULTA no ve fichas individuales.
		con := nuevoNavegador(t, ts.URL)
		con.entrarYCambiar("consulta", "ClaveProvisional1", "ClaveConsulta2027")
		if c, _ := con.get(fmt.Sprintf("/mujer/%d", mid)); c != 403 {
			t.Errorf("CONSULTA vio una ficha: %d", c)
		}
		if c, _ := con.get("/kpis?anio=2025"); c != 200 {
			t.Errorf("CONSULTA no ve KPIs: %d", c)
		}
		// AP: ve la ficha (auditado) y registra un VPH 16/18 -> derivación.
		ap := nuevoNavegador(t, ts.URL)
		ap.entrarYCambiar("enfermera", "ClaveProvisional1", "ClaveEnfermera2027")
		if c, body := ap.get(fmt.Sprintf("/mujer/%d", mid)); c != 200 || !strings.Contains(body, "W1") {
			t.Fatalf("AP no ve la ficha: %d", c)
		}
		if cuenta(t, b, `SELECT COUNT(*) FROM auditoria WHERE accion='ACCESO_FICHA' AND usuario='enfermera' AND objeto='W1'`) != 1 {
			t.Error("el acceso a la ficha no quedó auditado")
		}
		tok = ap.csrf()
		c, body := ap.post(fmt.Sprintf("/mujer/%d/prueba", mid), url.Values{"csrf": {tok}, "inv": {fmt.Sprint(inv)}, "tipo": {"VPH"},
			"finalidad": {"PRIMARIA"}, "muestra": {"PROF"}, "toma": {"2025-01-20"}, "fres": {"2025-01-30"}, "resultado": {"POS_1618"}})
		if c != 200 || !strings.Contains(body, "Prueba registrada") {
			t.Fatalf("registrar prueba: %d", c)
		}
		if c, _ := ap.post(fmt.Sprintf("/mujer/%d/colposcopia", mid), url.Values{"csrf": {tok}, "inv": {fmt.Sprint(inv)}}); c != 403 {
			t.Errorf("AP registró una colposcopia: %d", c)
		}
		var est, usu string
		b.QueryRow(`SELECT estado FROM invitaciones WHERE id=?`, inv).Scan(&est)
		b.QueryRow(`SELECT usuario FROM decisiones WHERE invitacion_id=?`, inv).Scan(&usu)
		if est != "DERIVADA_COLPOSCOPIA" || usu != "enfermera" {
			t.Errorf("tras VPH 16/18: estado %s, decisión de %q", est, usu)
		}
		// Ginecología registra la colposcopia con CIN3 -> seguimiento clínico.
		gi := nuevoNavegador(t, ts.URL)
		gi.entrarYCambiar("gine", "ClaveProvisional1", "ClaveGinecologia2027")
		if _, body := gi.get("/pendientes"); !strings.Contains(body, "Realizar colposcopia") {
			t.Error("Ginecología no ve la colposcopia pendiente")
		}
		c, _ = gi.post(fmt.Sprintf("/mujer/%d/colposcopia", mid), url.Values{"csrf": {gi.csrf()}, "inv": {fmt.Sprint(inv)}, "estado": {"REALIZADA"},
			"fecha": {"2025-02-20"}, "adecuada": {"S"}, "biopsia": {"S"}, "histologia": {"HSIL_CIN3"}, "fhist": {"2025-03-01"}})
		if c != 200 {
			t.Fatalf("colposcopia: %d", c)
		}
		var em string
		b.QueryRow(`SELECT estado FROM mujeres WHERE id=?`, mid).Scan(&em)
		if em != "SEGUIMIENTO_CLINICO" {
			t.Errorf("tras CIN3 la mujer está en %s", em)
		}
		// Coordinación descarga un lote: queda como SALIDA y en auditoría.
		co := nuevoNavegador(t, ts.URL)
		co.entrarYCambiar("coord", "ClaveProvisional1", "ClaveCoordina2027")
		if c, body := co.get("/lotes/exportar?lote=LW"); c != 200 || !strings.Contains(body, "W1") {
			t.Errorf("exportar lote: %d", c)
		}
		if cuenta(t, b, `SELECT COUNT(*) FROM cargas WHERE sentido='SALIDA' AND usuario='coord'`) != 1 ||
			cuenta(t, b, `SELECT COUNT(*) FROM auditoria WHERE accion='LOTE_EXPORTADO' AND usuario='coord'`) != 1 {
			t.Error("la exportación no quedó registrada")
		}
		if c, _ := co.get("/auditoria"); c != 403 {
			t.Errorf("COORDINACION vio la auditoría: %d", c)
		}
		// Cambio de parámetro por la web -> nueva versión.
		v0 := b.verReglas()
		co.post("/parametros", url.Values{"csrf": {co.csrf()}, "clave": {"plazo_colposcopia_dias"}, "valor": {"45"}, "motivo": {"instrucción 2027"}})
		if b.verReglas() != v0+1 || b.pInt("plazo_colposcopia_dias") != 45 {
			t.Error("el cambio de parámetro por la web no creó versión")
		}
		// Bloqueo tras 5 intentos fallidos.
		x := nuevoNavegador(t, ts.URL)
		for i := 0; i < 5; i++ {
			x.entrar("consulta", "incorrecta")
		}
		if _, body := x.entrar("consulta", "ClaveConsulta2027"); !strings.Contains(body, "bloqueado") {
			t.Error("no se bloqueó tras 5 intentos fallidos")
		}
		// Admin ve la auditoría y en ella el acceso de la enfermera.
		if c, body := admin.get("/auditoria?usuario=enfermera"); c != 200 || !strings.Contains(body, "ACCESO_FICHA") {
			t.Errorf("auditoría: %d", c)
		}
		// Salir invalida la sesión.
		ap.post("/salir", url.Values{"csrf": {ap.csrf()}})
		r, _ = ap.c.Get(ts.URL + "/pendientes")
		if r.Request.URL.Path != "/entrar" {
			t.Error("tras salir la sesión seguía viva")
		}
		r.Body.Close()
	})
}

func TestZonas(t *testing.T) {
	enMotores(t, func(t *testing.T, b *BD, dir string) {
		pg := Programa{b, "test", "TEST"}
		// El padrón trae tres zonas (una escrita de dos formas) y una mujer sin zona.
		csv := padronCSV(t, dir, [][]string{
			{"Z1", "1", "1980-01-01", "ZBS01"}, {"Z2", "2", "1981-01-01", "zbs01 "}, {"Z3", "3", "1982-01-01", "ZBS02"},
			{"Z4", "4", "1983-01-01", "NORTE-7"}, {"Z5", "5", "1984-01-01", ""}})
		if _, err := pg.cargarPadron(csv, d("2025-01-01"), "TEST"); err != nil {
			t.Fatal(err)
		}
		cat, faltan, sinZona, err := b.listarZonas()
		if err != nil || len(cat) != 0 || len(faltan) != 3 || sinZona != 1 {
			t.Fatalf("catálogo vacío: cat=%d faltan=%d sinZona=%d err=%v", len(cat), len(faltan), sinZona, err)
		}
		// CSV: una zona completa, una solo con código y dos filas erróneas.
		ruta := filepath.Join(dir, "zbs.csv")
		if err := escribirCSV(ruta, cabZonas, [][]string{
			{"zbs01", "Centro", "Área I", "CS Centro", "26089", "120", "S", "matrona L-X"},
			{"ZBS02", "", "", "", "", "", "", ""},
			{"ZBS03", "Mala", "", "", "", "muchas", "", ""},
			{"", "Sin código", "", "", "", "", "", ""}}); err != nil {
			t.Fatal(err)
		}
		rs, err := pg.importarZonas(ruta)
		if err != nil || rs.Aplicadas != 2 || rs.Rechazadas != 2 {
			t.Fatalf("importar zonas: %+v %v", rs, err)
		}
		cat, faltan, _, _ = b.listarZonas()
		if len(cat) != 2 || len(faltan) != 1 || faltan[0].Codigo != "NORTE-7" {
			t.Fatalf("tras importar: cat=%d faltan=%+v", len(cat), faltan)
		}
		if z := cat[0]; z.Codigo != "ZBS01" || z.Nombre != "Centro" || z.Cupo.Int64 != 120 || !z.Activa || z.Mujeres != 2 {
			t.Errorf("ZBS01: %+v", z)
		}
		if z := cat[1]; z.Nombre != "ZBS02" || z.Cupo.Valid || z.Mujeres != 1 {
			t.Errorf("ZBS02: %+v", z)
		}
		// Guardar un código existente actualiza, no duplica; se puede quitar el cupo.
		z, err := zonaDeCampos(" zbs01", "Centro histórico", "Área I", "", "", "", "N", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := pg.guardarZona(b, z); err != nil {
			t.Fatal(err)
		}
		g, err := b.zonaPorCodigo("ZBS01")
		if err != nil || g.Nombre != "Centro histórico" || g.Cupo.Valid || g.Activa || cuenta(t, b, `SELECT COUNT(*) FROM zbs`) != 2 {
			t.Errorf("actualización: %+v %v", g, err)
		}
		// Las zonas del padrón que faltan se catalogan sin tocar las demás.
		if n, err := pg.altaZonasDelPadron(); err != nil || n != 1 {
			t.Errorf("alta desde padrón: %d %v", n, err)
		}
		if _, faltan, _, _ = b.listarZonas(); len(faltan) != 0 || cuenta(t, b, `SELECT COUNT(*) FROM zbs WHERE nombre='Centro histórico'`) != 1 {
			t.Error("el alta desde el padrón dejó zonas sin catalogar o pisó una existente")
		}

		// Web: Coordinación gestiona las zonas; AP no.
		for _, u := range [][2]string{{"coord", "COORDINACION"}, {"enfermera", "AP"}} {
			if err := b.crearUsuario(u[0], u[0], u[1], "ClaveProvisional1", "t", "TEST"); err != nil {
				t.Fatal(err)
			}
		}
		sv, err := nuevoServidor(b, false, false)
		if err != nil {
			t.Fatal(err)
		}
		ts := httptest.NewServer(sv.rutas())
		defer ts.Close()
		ap := nuevoNavegador(t, ts.URL)
		ap.entrarYCambiar("enfermera", "ClaveProvisional1", "ClaveEnfermera2027")
		if c, _ := ap.get("/zonas"); c != 403 {
			t.Errorf("AP vio las zonas: %d", c)
		}
		co := nuevoNavegador(t, ts.URL)
		co.entrarYCambiar("coord", "ClaveProvisional1", "ClaveCoordina2027")
		if c, body := co.get("/zonas?editar=zbs02"); c != 200 || !strings.Contains(body, "Modificar la zona ZBS02") {
			t.Errorf("página de zonas: %d", c)
		}
		tok := co.csrf()
		if c, body := co.post("/zonas", url.Values{"csrf": {tok}, "codigo": {"sur 3"}, "nombre": {"Sur"}, "cupo_mensual": {"80"}, "activa": {"S"}}); c != 200 || !strings.Contains(body, "Zona SUR 3 guardada") {
			t.Errorf("alta a mano: %d", c)
		}
		if _, body := co.post("/zonas", url.Values{"csrf": {tok}, "codigo": {"X"}, "nombre": {"X"}, "cupo_mensual": {"-5"}}); !strings.Contains(body, "entero no negativo") {
			t.Error("aceptó un cupo negativo")
		}
		if cuenta(t, b, `SELECT COUNT(*) FROM zbs WHERE codigo='SUR 3' AND cupo_mensual=80 AND usuario='coord'`) != 1 ||
			cuenta(t, b, `SELECT COUNT(*) FROM auditoria WHERE accion='ZBS_GUARDADA' AND usuario='coord' AND objeto='SUR 3'`) != 1 {
			t.Error("el alta a mano no quedó guardada o auditada")
		}
		// Subida de un CSV por la web: una fila válida y una rechazada.
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		mw.WriteField("csrf", tok)
		fw, _ := mw.CreateFormFile("fichero", "mis_zonas.csv")
		fw.Write([]byte("\ufeffCodigo;Nombre;cupo_mensual\nESTE-1;Este;40\nESTE-2;Otra;x\n"))
		mw.Close()
		resp, err := co.c.Post(ts.URL+"/zonas/importar", mw.FormDataContentType(), &buf)
		if err != nil {
			t.Fatal(err)
		}
		cuerpo, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(cuerpo), "Importación terminada") || !strings.Contains(string(cuerpo), "cupo_mensual debe ser") ||
			cuenta(t, b, `SELECT COUNT(*) FROM zbs WHERE codigo='ESTE-1' AND cupo_mensual=40`) != 1 || cuenta(t, b, `SELECT COUNT(*) FROM zbs WHERE codigo='ESTE-2'`) != 0 {
			t.Errorf("subida de CSV por la web: %d", resp.StatusCode)
		}
		if c, body := co.get("/zonas.csv"); c != 200 || !strings.HasPrefix(body, "codigo;nombre;area;centro;municipio;cupo_mensual;activa;notas") || !strings.Contains(body, "SUR 3;Sur;;;;80;S;") {
			t.Errorf("descarga del catálogo: %d\n%s", c, body)
		}
	})
}
