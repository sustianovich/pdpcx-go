package main

import (
	"database/sql"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestWebExclusionesRegression(t *testing.T) {
	for _, caso := range []struct {
		nombre, tipo, fin, errorEsperado string
	}{
		{"definitiva fin no vacio", "DEFINITIVA", "2025-02-01", ""},
		{"definitiva fin anterior", "DEFINITIVA", "2024-12-01", ""},
		{"definitiva fin malformado", "DEFINITIVA", "no-es-fecha", ""},
		{"temporal sin fin", "TEMPORAL", "", "necesita fecha_fin"},
		{"temporal fin anterior", "TEMPORAL", "2024-12-01", "el fin es anterior al inicio"},
		{"temporal fin malformado", "TEMPORAL", "no-es-fecha", "fecha"},
		{"temporal valido", "TEMPORAL", "2025-02-01", ""},
	} {
		t.Run(caso.nombre, func(t *testing.T) {
			b, _, id := prepararExclusionRegression(t)
			sv, err := nuevoServidor(b, false, false)
			if err != nil {
				t.Fatal(err)
			}
			s := &Sesion{Usuario: &Usuario{Login: "operador-web", Rol: "ADMISION"}}
			valores := url.Values{"tipo": {caso.tipo}, "inicio": {"2025-01-10"}, "fin": {caso.fin}, "motivo": {"OTRO"}}
			r := httptest.NewRequest("POST", fmt.Sprintf("/mujer/%d/exclusion", id), strings.NewReader(valores.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.SetPathValue("id", fmt.Sprint(id))
			w := httptest.NewRecorder()
			antes := time.Now().Add(-time.Second)
			sv.accionExclusion(w, r, s)
			despues := time.Now()
			if caso.errorEsperado != "" {
				if !strings.Contains(w.Body.String(), caso.errorEsperado) || strings.Contains(w.Body.String(), "Exclusión registrada.") {
					t.Fatalf("respuesta sin error esperado %q", caso.errorEsperado)
				}
				if cuenta(t, b, `SELECT COUNT(*) FROM exclusiones`) != 0 || cuenta(t, b, `SELECT COUNT(*) FROM eventos WHERE tipo LIKE 'EXCLUSION_%'`) != 0 {
					t.Fatal("el error HTTP dejó exclusión o evento")
				}
				return
			}
			if !strings.Contains(w.Body.String(), "Exclusión registrada.") {
				t.Fatal("no se registró la exclusión HTTP")
			}
			var fin sql.NullString
			if err := b.QueryRow(`SELECT fecha_fin FROM exclusiones WHERE mujer_id=?`, id).Scan(&fin); err != nil {
				t.Fatal(err)
			}
			if caso.tipo == "DEFINITIVA" && fin.Valid {
				t.Fatalf("HTTP guardó Fin en definitiva: %v", fin)
			}
			if caso.tipo == "TEMPORAL" && (!fin.Valid || fin.String != caso.fin) {
				t.Fatalf("HTTP alteró Fin temporal: %v", fin)
			}
			f, err := b.cargarFicha(id)
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, e := range f.Eventos {
				if e.Tipo != "EXCLUSION_"+caso.tipo {
					continue
				}
				n++
				registro, err := time.ParseInLocation("2006-01-02 15:04:05", e.Registrado, time.Local)
				if err != nil || registro.Before(antes) || registro.After(despues) || e.Fecha != "2025-01-10" || e.Usuario != "operador-web" || e.Origen != "WEB" || e.Detalle != "OTRO" {
					t.Fatalf("traza perdió fecha efectiva, registro real o autoría: %+v (%v)", e, err)
				}
				rGet := httptest.NewRequest("GET", fmt.Sprintf("/mujer/%d", id), nil)
				rGet.SetPathValue("id", fmt.Sprint(id))
				get := httptest.NewRecorder()
				sv.ficha(get, rGet, s)
				fila := fmt.Sprintf("<td>%s</td><td></td><td>%s</td><td>OTRO</td><td>WEB</td><td>operador-web</td><td class=\"pequeno\">%s</td>", e.Fecha, e.Tipo, e.Registrado)
				if get.Code != 200 || !strings.Contains(get.Body.String(), fila) {
					t.Fatal("la ficha no renderizó la exclusión con ambas fechas y autoría")
				}
			}
			if n != 1 {
				t.Fatalf("se esperaba un evento de exclusión, hay %d", n)
			}
		})
	}

	t.Run("formulario sin JavaScript y fechas explicadas", func(t *testing.T) {
		b, _, id := prepararExclusionRegression(t)
		sv, err := nuevoServidor(b, false, false)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("GET", fmt.Sprintf("/mujer/%d", id), nil)
		r.SetPathValue("id", fmt.Sprint(id))
		w := httptest.NewRecorder()
		sv.ficha(w, r, &Sesion{Usuario: &Usuario{Login: "operador-web", Rol: "ADMISION"}})
		html := w.Body.String()
		for _, texto := range []string{`class="linea formulario-exclusion"`, `<option value="DEFINITIVA">DEFINITIVA</option>`, `class="fin-temporal"`, `name="fin">`, `href="/estatico/estilo.css"`, `Fecha efectiva`, `Registrado`, `ordenada por fecha efectiva`, `no por el momento de registro`} {
			if !strings.Contains(html, texto) {
				t.Errorf("falta contrato visible del formulario/traza: %s", texto)
			}
		}
		if strings.Contains(html, "<script") || strings.Contains(html, "onchange=") {
			t.Error("el formulario no debe necesitar JavaScript")
		}
		css, err := webFS.ReadFile("web/estilo.css")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(css), `.formulario-exclusion:has(select[name="tipo"] option[value="DEFINITIVA"]:checked) .fin-temporal{display:none}`) {
			t.Error("falta regla CSS externa limitada al formulario de exclusión")
		}
	})
}
