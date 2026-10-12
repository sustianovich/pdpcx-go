package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func prepararCalendario(t *testing.T, b *BD, dir string) Programa {
	t.Helper()
	pg := Programa{b, "test-mensual", "TEST"}
	personas := [][]string{
		{"C29", "1", "1997-02-01"}, {"V30", "2", "1997-01-10"}, {"V65", "3", "1961-02-01"},
		{"OUT66", "4", "1961-01-20"}, {"LATE", "5", "1980-01-01"}, {"CTRL", "6", "1980-01-01"},
		{"OLD", "7", "1980-01-01"}, {"EXCL", "8", "1980-01-01"}, {"FEB", "9", "1980-01-01"},
	}
	if _, err := pg.cargarPadron(padronCSV(t, dir, personas), d("2026-01-01"), "TEST"); err != nil {
		t.Fatal(err)
	}
	for cip, fecha := range map[string]string{
		"C29": "2027-01-20", "V30": "2027-01-10", "V65": "2027-01-20", "OUT66": "2027-01-20",
		"LATE": "2026-12-12", "CTRL": "2026-01-12", "OLD": "2026-01-10", "EXCL": "2027-01-20", "FEB": "2027-02-05",
	} {
		if _, err := b.Exec(`UPDATE mujeres SET fecha_proxima=?,motivo_proxima='RUTINA' WHERE cip=?`, fecha, cip); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pg.planificar(d("2026-01-01"), d("2026-01-31"), "ANTERIOR", "TEST"); err != nil {
		t.Fatal(err)
	}
	var inv int64
	if err := b.QueryRow(`SELECT i.id FROM invitaciones i JOIN mujeres m ON m.id=i.mujer_id WHERE m.cip='CTRL'`).Scan(&inv); err != nil {
		t.Fatal(err)
	}
	txP0(t, b, func(tx *Tx) error {
		if _, err := pg.registrarPrueba(tx, inv, "VPH", "PRIMARIA", "PROF", d("2026-01-15"), d("2026-01-20"), "POS_OTROS", "TEST"); err != nil {
			return err
		}
		_, err := pg.registrarPrueba(tx, inv, "CITOLOGIA", "TRIAJE", "PROF", d("2026-01-15"), d("2026-01-25"), "NEGATIVA", "TEST")
		return err
	})
	if _, err := b.Exec(`INSERT INTO exclusiones(mujer_id,tipo,motivo,fecha_inicio,fecha_fin,origen)
	                  SELECT id,'TEMPORAL','OTRO','2027-01-15','2027-02-15','TEST' FROM mujeres WHERE cip='EXCL'`); err != nil {
		t.Fatal(err)
	}
	return pg
}

func cuentasCalendario(t *testing.T, b *BD) []int {
	t.Helper()
	var ns []int
	for _, tabla := range []string{"invitaciones", "pruebas", "decisiones", "eventos", "cargas", "planificacion_mensual"} {
		ns = append(ns, cuenta(t, b, `SELECT COUNT(*) FROM `+tabla))
	}
	return ns
}

func TestPrevisionMensual(t *testing.T) {
	enMotores(t, func(t *testing.T, b *BD, dir string) {
		pg := prepararCalendario(t, b, dir)
		antes := cuentasCalendario(t, b)
		cal, err := pg.calendarioMensual(2027, d("2027-01-15"))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(antes, cuentasCalendario(t, b)) {
			t.Fatal("la previsión escribió en la base")
		}
		if len(cal.Meses) != 12 || cal.Atrasadas != 2 || cal.Proximo != "2027-01" {
			t.Fatalf("calendario incorrecto: %+v", cal)
		}
		enero := cal.Meses[0]
		want := conteoMes{Citologia: 1, VPH: 3, Controles: 1, Reinvitaciones: 1, Salidas: 1}
		if enero.conteoMes != want || !enero.PuedeGenerar || enero.Generado {
			t.Fatalf("previsión enero: %+v", enero)
		}
		if cal.Meses[1].VPH != 2 || cal.Meses[1].PuedeGenerar {
			t.Fatalf("previsión febrero: %+v", cal.Meses[1])
		}
		for _, m := range cal.Meses[2:] {
			if m.Total() != 0 {
				t.Fatalf("cita contada en varios meses: %+v", m)
			}
		}
		rs, err := pg.generarMes(2027, 1, "TEST")
		if err != nil {
			t.Fatal(err)
		}
		if rs.CerradasSinRespuesta != 1 || rs.AplazadasExclusion != 1 || rs.SalenPorEdad != 1 {
			t.Fatalf("generación: %+v", rs)
		}
		cal, err = pg.calendarioMensual(2027, d("2027-01-15"))
		if err != nil {
			t.Fatal(err)
		}
		if !cal.Meses[0].Generado || cal.Meses[0].conteoMes != want || !cal.Meses[1].PuedeGenerar {
			t.Fatalf("previsión no coincide con lote real: %+v", cal.Meses)
		}
		for cip, prueba := range map[string]string{"C29": "CITOLOGIA", "V30": "VPH", "V65": "VPH"} {
			if cuenta(t, b, `SELECT COUNT(*) FROM invitaciones i JOIN mujeres m ON m.id=i.mujer_id WHERE i.lote='2027-01' AND m.cip=? AND i.prueba=?`, cip, prueba) != 1 {
				t.Fatalf("prueba incorrecta para %s", cip)
			}
		}
		if cuenta(t, b, `SELECT COUNT(*) FROM invitaciones i JOIN mujeres m ON m.id=i.mujer_id WHERE i.lote='2027-01' AND m.cip='LATE' AND i.fecha_invitacion='2027-01-01'`) != 1 {
			t.Fatal("no se incorporó la cita atrasada al inicio del mes")
		}
		if cuenta(t, b, `SELECT COUNT(*) FROM invitaciones WHERE lote='2027-01' AND tipo='CONTROL' AND episodio_origen IS NOT NULL`) != 1 {
			t.Fatal("el control perdió el episodio de origen")
		}
		antes = cuentasCalendario(t, b)
		for _, mes := range []int{1, 3} {
			if _, err := pg.generarMes(2027, mes, "TEST"); err == nil {
				t.Fatalf("aceptó mes duplicado o salto: %d", mes)
			}
		}
		if !reflect.DeepEqual(antes, cuentasCalendario(t, b)) {
			t.Fatal("un mes rechazado dejó cambios")
		}
		if _, err := pg.generarMes(2027, 2, "TEST"); err != nil {
			t.Fatal(err)
		}
		cal, err = pg.calendarioMensual(2027, d("2027-01-15"))
		if err != nil || !cal.Meses[1].Generado || cal.Meses[1].Total() != 2 {
			t.Fatalf("febrero: %+v, %v", cal.Meses[1], err)
		}
	})
}

func TestMesVacioYOrdenAnual(t *testing.T) {
	enMotores(t, func(t *testing.T, b *BD, dir string) {
		pg := Programa{b, "test", "TEST"}
		if _, err := pg.generarMes(2030, 2, "TEST"); err == nil {
			t.Fatal("permitió empezar por febrero")
		}
		for mes := 1; mes <= 12; mes++ {
			if _, err := pg.generarMes(2030, mes, "TEST"); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := pg.generarMes(2030, 12, "TEST"); err == nil {
			t.Fatal("permitió repetir un mes vacío")
		}
		if _, err := pg.generarMes(2032, 1, "TEST"); err == nil {
			t.Fatal("permitió saltarse un año")
		}
		cal, err := pg.calendarioMensual(2031, d("2026-10-11"))
		if err != nil || cal.Proximo != "2031-01" || !cal.Meses[0].PuedeGenerar {
			t.Fatalf("cambio de año: %+v, %v", cal, err)
		}
		if _, err := pg.generarMes(2031, 1, "TEST"); err != nil {
			t.Fatalf("no permitió un mes futuro: %v", err)
		}
		if cuenta(t, b, `SELECT COUNT(*) FROM planificacion_mensual`) != 13 {
			t.Fatal("no registró los meses vacíos")
		}
		if _, err := pg.planificar(d("2031-02-01"), d("2031-02-28"), "2031-02", "TEST"); err == nil {
			t.Fatal("el periodo libre eludió el orden mensual")
		}
	})
}

func TestPrevisionMensualOtroAnio(t *testing.T) {
	enMotores(t, func(t *testing.T, b *BD, dir string) {
		pg := Programa{b, "test", "TEST"}
		if _, err := pg.cargarPadron(padronCSV(t, dir, [][]string{{"FUTURA", "1", "1980-01-01"}}), d("2026-01-01"), "TEST"); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Exec(`UPDATE mujeres SET fecha_proxima='2027-02-10'`); err != nil {
			t.Fatal(err)
		}
		if _, err := pg.generarMes(2027, 1, "TEST"); err != nil {
			t.Fatal(err)
		}
		cal, err := pg.calendarioMensual(2028, d("2026-10-11"))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range cal.Meses {
			if m.Total() != 0 || m.PuedeGenerar {
				t.Fatalf("arrastró una cita que corresponde al año anterior: %+v", m)
			}
		}
	})
}

func TestCalendarioMensualDiezMil(t *testing.T) {
	enMotores(t, func(t *testing.T, b *BD, dir string) {
		pg := Programa{b, "test", "TEST"}
		ruta := filepath.Join(dir, "padron10k.csv")
		if err := generarPadron(ruta, 10000, 2027, 42); err != nil {
			t.Fatal(err)
		}
		if _, err := pg.cargarPadron(ruta, d("2027-01-01"), "TEST"); err != nil {
			t.Fatal(err)
		}
		// La agenda sintética cubre los doce meses y evita la salida por edad.
		if _, err := b.Exec(`UPDATE mujeres SET fecha_nacimiento='1980-01-01'`); err != nil {
			t.Fatal(err)
		}
		for mes := 1; mes <= 12; mes++ {
			if _, err := b.Exec(`UPDATE mujeres SET fecha_proxima=? WHERE id % 12=?`, fmt.Sprintf("2027-%02d-15", mes), mes-1); err != nil {
				t.Fatal(err)
			}
		}
		cal, err := pg.calendarioMensual(2027, d("2027-01-01"))
		if err != nil {
			t.Fatal(err)
		}
		total := 0
		for _, m := range cal.Meses {
			total += m.Total()
		}
		if total != 10000 {
			t.Fatalf("calendario de 10.000 mujeres: %d invitaciones", total)
		}
	})
}

func TestGeneracionMensualConcurrente(t *testing.T) {
	enMotores(t, func(t *testing.T, b *BD, dir string) {
		pg := Programa{b, "test", "TEST"}
		otro := pg
		if !b.pg {
			b2, err := abrirBD(filepath.Join(dir, "t.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { b2.Close() })
			otro.BD = b2
		}
		inicio := make(chan struct{})
		resultados := make(chan error, 2)
		for _, p := range []Programa{pg, otro} {
			go func(p Programa) { <-inicio; _, err := p.generarMes(2027, 1, "TEST"); resultados <- err }(p)
		}
		close(inicio)
		exitos := 0
		for i := 0; i < 2; i++ {
			if err := <-resultados; err == nil {
				exitos++
			} else if !strings.Contains(err.Error(), "ya está generado") {
				t.Fatalf("error inesperado: %v", err)
			}
		}
		if exitos != 1 || cuenta(t, b, `SELECT COUNT(*) FROM planificacion_mensual`) != 1 || cuenta(t, b, `SELECT COUNT(*) FROM cargas WHERE tipo='PLANIFICACION'`) != 1 {
			t.Fatal("la generación simultánea no fue única")
		}
	})
}

func TestIntervalosYTransicionMensual(t *testing.T) {
	enMotores(t, func(t *testing.T, b *BD, dir string) {
		pg := Programa{b, "test", "TEST"}
		if _, err := pg.cargarPadron(padronCSV(t, dir, [][]string{{"TRANSICION", "1", "1998-01-01"}}), d("2026-01-01"), "TEST"); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Exec(`UPDATE mujeres SET fecha_proxima='2026-02-10'`); err != nil {
			t.Fatal(err)
		}
		if _, err := pg.planificar(d("2026-02-01"), d("2026-02-28"), "CITO_INICIAL", "TEST"); err != nil {
			t.Fatal(err)
		}
		var inv int64
		if err := b.QueryRow(`SELECT id FROM invitaciones`).Scan(&inv); err != nil {
			t.Fatal(err)
		}
		txP0(t, b, func(tx *Tx) error {
			_, err := pg.registrarPrueba(tx, inv, "CITOLOGIA", "PRIMARIA", "PROF", d("2026-02-10"), d("2026-02-20"), "NEGATIVA", "TEST")
			return err
		})
		cal, err := pg.calendarioMensual(2029, d("2026-10-11"))
		if err != nil || cal.Meses[1].VPH != 1 || cal.Meses[1].Citologia != 0 {
			t.Fatalf("citología a los 28 no pasó a VPH a los 31: %+v, %v", cal, err)
		}
		for mes := 1; mes <= 2; mes++ {
			if _, err := pg.generarMes(2029, mes, "TEST"); err != nil {
				t.Fatal(err)
			}
		}
		if err := b.QueryRow(`SELECT id FROM invitaciones WHERE lote='2029-02' AND prueba='VPH' AND fecha_invitacion='2029-02-10'`).Scan(&inv); err != nil {
			t.Fatal("no conservó el intervalo de 36 meses: ", err)
		}
		txP0(t, b, func(tx *Tx) error {
			_, err := pg.registrarPrueba(tx, inv, "VPH", "PRIMARIA", "PROF", d("2029-02-10"), d("2029-02-25"), "NEGATIVO", "TEST")
			return err
		})
		if cuenta(t, b, `SELECT COUNT(*) FROM mujeres WHERE fecha_proxima='2034-02-10'`) != 1 {
			t.Fatal("no conservó el intervalo VPH de 60 meses")
		}
		if err := b.fijarParametro("edad_vph_min", "31", "test", "TEST", "probar incoherencia"); err != nil {
			t.Fatal(err)
		}
		antes := cuentasCalendario(t, b)
		if _, err := pg.generarMes(2029, 3, "TEST"); err == nil || !strings.Contains(err.Error(), "rangos de edad incoherentes") {
			t.Fatalf("ignoró edad_vph_min: %v", err)
		}
		if !reflect.DeepEqual(antes, cuentasCalendario(t, b)) {
			t.Fatal("la configuración incoherente dejó cambios parciales")
		}
	})
}

func TestRollbackMensual(t *testing.T) {
	b, dir := bdDe(t, motorTest{"sqlite", ""})
	pg := prepararCalendario(t, b, dir)
	if _, err := b.Exec(`CREATE TRIGGER impedir_mes BEFORE INSERT ON planificacion_mensual BEGIN SELECT RAISE(ABORT,'fallo de prueba'); END`); err != nil {
		t.Fatal(err)
	}
	antes := cuentasCalendario(t, b)
	if _, err := pg.generarMes(2027, 1, "TEST"); err == nil {
		t.Fatal("no se provocó el fallo de escritura")
	}
	if !reflect.DeepEqual(antes, cuentasCalendario(t, b)) {
		t.Fatal("el fallo dejó invitaciones, eventos o cargas parciales")
	}
	if cuenta(t, b, `SELECT COUNT(*) FROM mujeres WHERE cip='OUT66' AND estado='ACTIVA'`) != 1 || cuenta(t, b, `SELECT COUNT(*) FROM invitaciones i JOIN mujeres m ON m.id=i.mujer_id WHERE m.cip='OLD' AND i.estado='ENVIADA'`) != 1 {
		t.Fatal("el rollback no restauró los estados")
	}
}

func TestWebPlanificacionMensual(t *testing.T) {
	enMotores(t, func(t *testing.T, b *BD, dir string) {
		prepararCalendario(t, b, dir)
		for login, rol := range map[string]string{"admin": "ADMIN", "coord": "COORDINACION", "ap": "AP"} {
			if err := b.crearUsuario(login, login, rol, "ClaveInicial2027", "test", "TEST"); err != nil {
				t.Fatal(err)
			}
		}
		sv, err := nuevoServidor(b, false, false)
		if err != nil {
			t.Fatal(err)
		}
		ts := httptest.NewServer(sv.rutas())
		defer ts.Close()
		admin := nuevoNavegador(t, ts.URL)
		admin.entrarYCambiar("admin", "ClaveInicial2027", "NuevaClaveAdmin2027")
		if c, body := admin.get("/planificacion?anio=2027"); c != 200 || !strings.Contains(body, "Diciembre 2027") || strings.Count(body, ">Generar lote</button>") != 1 {
			t.Fatalf("calendario web: %d, %s", c, body)
		}
		if c, _ := admin.post("/planificacion/generar", url.Values{"anio": {"2027"}, "mes": {"1"}}); c != http.StatusForbidden {
			t.Fatal("aceptó POST sin CSRF")
		}
		ap := nuevoNavegador(t, ts.URL)
		ap.entrarYCambiar("ap", "ClaveInicial2027", "NuevaClaveEnfermera2027")
		if c, _ := ap.get("/planificacion?anio=2027"); c != http.StatusForbidden {
			t.Fatal("AP pudo ver planificación")
		}
		if c, _ := ap.post("/planificacion/generar", url.Values{"csrf": {ap.csrf()}, "anio": {"2027"}, "mes": {"1"}}); c != http.StatusForbidden {
			t.Fatal("AP pudo generar el mes")
		}
		co := nuevoNavegador(t, ts.URL)
		co.entrarYCambiar("coord", "ClaveInicial2027", "NuevaClaveCoord2027")
		if c, body := co.post("/planificacion/generar", url.Values{"csrf": {co.csrf()}, "anio": {"2027"}, "mes": {"1"}}); c != 200 || !strings.Contains(body, "Lote 2027-01 generado: 6 invitaciones") || !strings.Contains(body, "lote=2027-01") {
			t.Fatalf("generar por coordinación: %d, %s", c, body)
		}
		if cuenta(t, b, `SELECT COUNT(*) FROM auditoria WHERE accion='LOTE_MENSUAL_GENERADO' AND usuario='coord' AND objeto='2027-01'`) != 1 {
			t.Fatal("generación sin auditoría")
		}
		if c, body := co.get("/lotes/exportar?lote=2027-01"); c != 200 || !strings.Contains(body, "C29") || !strings.Contains(body, "OLD") {
			t.Fatalf("CSV mensual incorrecto: %d", c)
		}
		if c, body := co.post("/planificacion/generar", url.Values{"csrf": {co.csrf()}, "anio": {"2027"}, "mes": {"1"}}); c != 200 || !strings.Contains(body, "ya está generado") {
			t.Fatal("no mostró el rechazo del mes duplicado")
		}
		if c, _ := admin.get("/planificacion?anio=abc"); c != http.StatusBadRequest {
			t.Fatal("aceptó año inválido")
		}
		if c, _ := admin.get("/lotes"); c != 200 {
			t.Fatal("el histórico dejó de funcionar")
		}
		if c, _ := admin.post("/planificacion/generar", url.Values{"csrf": {admin.csrf()}, "anio": {"2027"}, "mes": {"13"}}); c != http.StatusBadRequest {
			t.Fatal("aceptó mes inválido")
		}
	})
}
