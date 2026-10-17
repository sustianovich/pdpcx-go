package main

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// Aísla la cabecera real: las secciones no necesitan datos clínicos ni BD.
func renderNavegacion(t *testing.T, nombre, rol string) string {
	t.Helper()
	sv, err := nuevoServidor(nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sv.tmpl.New(nombre).Parse(`{{template "cabecera" .}}{{template "pie" .}}`); err != nil {
		t.Fatal(err)
	}
	var s *Sesion
	if rol != "" {
		s = &Sesion{Usuario: &Usuario{Nombre: "Prueba", Rol: rol}}
	}
	w := httptest.NewRecorder()
	sv.render(w, nombre, Pagina{Titulo: "Título ajeno a la sección", NavActiva: "/usuarios", S: s})
	if w.Header().Get("Content-Type") != "text/html; charset=utf-8" || !strings.Contains(w.Body.String(), "</html>") {
		t.Fatal("respuesta HTML incompleta")
	}
	if strings.Contains(w.Body.String(), "<script") {
		t.Fatal("la navegación no debe necesitar JavaScript")
	}
	return w.Body.String()
}

func enlacesNavegacion(t *testing.T, html string) (enlaces, activos []string) {
	t.Helper()
	inicio, fin := strings.Index(html, "<nav>"), strings.Index(html, "</nav>")
	if inicio < 0 || fin < inicio {
		t.Fatal("falta la navegación de la sesión")
	}
	for _, enlace := range regexp.MustCompile(`<a href="([^"]+)"([^>]*)>`).FindAllStringSubmatch(html[inicio:fin], -1) {
		enlaces = append(enlaces, enlace[1])
		if strings.Contains(enlace[2], `aria-current="page"`) {
			activos = append(activos, enlace[1])
		}
	}
	return
}

func TestNavegacionSecciones(t *testing.T) {
	for _, caso := range []struct{ plantilla, href string }{
		{"inicio", "/"}, {"buscar", "/buscar"}, {"ficha", "/buscar"},
		{"pendientes", "/pendientes"}, {"kpis", "/kpis"}, {"lotes", "/lotes"},
		{"planificacion", "/planificacion"}, {"importar", "/importar"},
		{"zonas", "/zonas"}, {"parametros", "/parametros"},
		{"usuarios", "/usuarios"}, {"auditoria", "/auditoria"},
		{"login", ""}, {"clave", ""}, {"error", ""}, {"desconocida", ""},
		{"buscar_extra", ""}, {"/buscar", ""},
	} {
		t.Run(caso.plantilla, func(t *testing.T) {
			_, activos := enlacesNavegacion(t, renderNavegacion(t, caso.plantilla, "ADMIN"))
			if caso.href == "" {
				if len(activos) != 0 {
					t.Fatalf("sección sin selección: %v", activos)
				}
			} else if len(activos) != 1 || activos[0] != caso.href {
				t.Fatalf("selección = %v; se esperaba solo %s", activos, caso.href)
			}
		})
	}
}

func TestNavegacionEstiloExterno(t *testing.T) {
	css, err := webFS.ReadFile("web/estilo.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, regla := range []string{
		`header nav{display:flex;flex-wrap:wrap;`,
		`header nav a[aria-current="page"]{background:#fff;color:var(--acento);font-weight:700;opacity:1}`,
		`header nav a:focus-visible{outline:2px solid #fff;outline-offset:2px;`,
	} {
		if !strings.Contains(string(css), regla) {
			t.Errorf("falta estilo de navegación externo: %s", regla)
		}
	}
}

func TestNavegacionPermisos(t *testing.T) {
	for _, caso := range []struct {
		rol, plantilla, visibles, activo string
	}{
		{"CONSULTA", "kpis", "/,/kpis", "/kpis"},
		{"CONSULTA", "ficha", "/,/kpis", ""},
		{"ADMISION", "ficha", "/,/buscar,/pendientes", "/buscar"},
		{"ADMISION", "planificacion", "/,/buscar,/pendientes", ""},
		{"LABORATORIO", "importar", "/,/buscar,/pendientes,/importar", "/importar"},
		{"COORDINACION", "lotes", "/,/buscar,/pendientes,/kpis,/lotes,/planificacion,/importar,/zonas,/parametros", "/lotes"},
		{"COORDINACION", "planificacion", "/,/buscar,/pendientes,/kpis,/lotes,/planificacion,/importar,/zonas,/parametros", "/planificacion"},
		{"COORDINACION", "usuarios", "/,/buscar,/pendientes,/kpis,/lotes,/planificacion,/importar,/zonas,/parametros", ""},
	} {
		t.Run(caso.rol+"/"+caso.plantilla, func(t *testing.T) {
			enlaces, activos := enlacesNavegacion(t, renderNavegacion(t, caso.plantilla, caso.rol))
			if strings.Join(enlaces, ",") != caso.visibles {
				t.Errorf("permisos alterados: %v", enlaces)
			}
			if strings.Join(activos, ",") != caso.activo {
				t.Errorf("selección = %v; se esperaba %q", activos, caso.activo)
			}
		})
	}
	if html := renderNavegacion(t, "login", ""); strings.Contains(html, "<nav>") || strings.Contains(html, "aria-current=") {
		t.Error("la sesión anónima no debe mostrar navegación ni selección")
	}
}
