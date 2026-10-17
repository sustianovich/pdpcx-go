package main

import (
	"database/sql"
	"testing"
	"time"
)

func prepararExclusionRegression(t *testing.T) (*BD, Programa, int64) {
	t.Helper()
	b, dir := bdDe(t, motorTest{nombre: "sqlite"})
	pg := Programa{BD: b, Usuario: "operador-exclusion", Canal: "TEST"}
	ruta := padronCSV(t, dir, [][]string{{"EXCLUSION-SINTETICA", "100", "1980-01-01"}})
	if _, err := pg.cargarPadron(ruta, d("2025-01-01"), "TEST"); err != nil {
		t.Fatal(err)
	}
	id, err := b.buscarMujer("EXCLUSION-SINTETICA")
	if err != nil {
		t.Fatal(err)
	}
	return b, pg, id
}

func TestExclusionesRegression(t *testing.T) {
	for _, caso := range []struct {
		nombre, tipo, fin string
		invalida          bool
	}{
		{"definitiva sin fin", "DEFINITIVA", "", false},
		{"definitiva descarta fin anterior", "DEFINITIVA", "2024-12-01", false},
		{"temporal necesita fin", "TEMPORAL", "", true},
		{"temporal fin anterior", "TEMPORAL", "2024-12-01", true},
		{"temporal mismo dia", "TEMPORAL", "2025-01-10", false},
	} {
		t.Run(caso.nombre, func(t *testing.T) {
			b, pg, id := prepararExclusionRegression(t)
			var fin time.Time
			if caso.fin != "" {
				fin = d(caso.fin)
			}
			tx, err := b.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			err = pg.crearExclusion(tx, id, caso.tipo, "OTRO", d("2025-01-10"), fin, "TEST")
			if caso.invalida {
				if err == nil {
					t.Fatal("se aceptó una exclusión temporal sin fechas ordenadas")
				}
				tx.Rollback()
				if cuenta(t, b, `SELECT COUNT(*) FROM exclusiones`) != 0 || cuenta(t, b, `SELECT COUNT(*) FROM eventos WHERE tipo LIKE 'EXCLUSION_%'`) != 0 {
					t.Fatal("la exclusión rechazada dejó datos")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			var guardado sql.NullString
			if err := b.QueryRow(`SELECT fecha_fin FROM exclusiones WHERE mujer_id=?`, id).Scan(&guardado); err != nil {
				t.Fatal(err)
			}
			if caso.tipo == "DEFINITIVA" && guardado.Valid {
				t.Fatalf("Fin de definitiva debe ser NULL, recibido %v", guardado)
			}
			if caso.tipo == "TEMPORAL" && (!guardado.Valid || guardado.String != caso.fin) {
				t.Fatalf("Fin temporal alterado: %v", guardado)
			}
		})
	}
}
