package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestPendientesOpciones(t *testing.T) {
	sv, err := nuevoServidor(nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	renderSelector := func(t *testing.T, plantilla, campo string, datos any) string {
		t.Helper()
		var html bytes.Buffer
		if err := sv.tmpl.ExecuteTemplate(&html, plantilla, Pagina{
			S: &Sesion{Usuario: &Usuario{Nombre: "Prueba", Rol: "ADMIN"}}, D: datos,
		}); err != nil {
			t.Fatal(err)
		}
		_, selector, ok := strings.Cut(html.String(), `<select name="`+campo+`">`)
		if !ok {
			t.Fatalf("falta selector %s", campo)
		}
		selector, _, ok = strings.Cut(selector, "</select>")
		if !ok {
			t.Fatal("selector incompleto")
		}
		return selector
	}
	for _, servicio := range []string{"", "AP", "COORDINACION"} {
		t.Run("pendientes/"+servicio, func(t *testing.T) {
			selector := renderSelector(t, "pendientes", "servicio", map[string]any{
				"PuedeElegir": true, "Servicios": servicios, "Servicio": servicio,
				"Tareas": []Tarea{}, "Vencidas": 0,
			})
			if strings.Contains(selector, "COORDINACION") {
				t.Error("Pendientes no debe ofrecer COORDINACION")
			}
			if !strings.Contains(selector, `<option value="">Todos</option>`) {
				t.Error("falta Todos")
			}
			for _, otro := range []string{"ADMISION", "AP", "LABORATORIO", "GINECOLOGIA"} {
				opcion := "<option >" + otro + "</option>"
				if otro == servicio {
					opcion = "<option selected>" + otro + "</option>"
				}
				if !strings.Contains(selector, opcion) {
					t.Errorf("falta opción o selección: %s", opcion)
				}
			}
		})
	}
	t.Run("usuarios conserva rol", func(t *testing.T) {
		selector := renderSelector(t, "usuarios", "rol", map[string]any{"Roles": roles})
		if !strings.Contains(selector, "<option>COORDINACION</option>") {
			t.Error("Usuarios debe conservar el rol COORDINACION")
		}
	})
}
