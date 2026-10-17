package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// v0.35.6 uses wasm2go, not a selectable wazero engine. Check the observable
// production connection contract instead of deleted private runtime fields.
func TestSQLiteWASMRegressionRuntimeConfig(t *testing.T) {
	b, _ := bdDe(t, motorTest{nombre: "sqlite"})
	if got := b.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("SQLite pool=%d; want 1", got)
	}
	for _, check := range []struct{ sql, want string }{
		{"PRAGMA journal_mode", "delete"},
		{"PRAGMA foreign_keys", "1"},
		{"PRAGMA integrity_check", "ok"},
	} {
		var got string
		if err := b.QueryRow(check.sql).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != check.want {
			t.Errorf("%s=%q; want %q", check.sql, got, check.want)
		}
	}
	var version string
	if err := b.QueryRow("SELECT sqlite_version()").Scan(&version); err != nil || version == "" {
		t.Fatalf("SQLite runtime unavailable: version=%q err=%v", version, err)
	}
	t.Logf("backend=wasm2go SQLite=%s; default runtime, pool=1, journal=delete", version)
}

func sqliteWASMFixture(t *testing.T) (*BD, map[string]string, [][6]string) {
	t.Helper()
	b, _ := bdDe(t, motorTest{nombre: "sqlite"})
	tx, err := b.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	states := []string{"ACTIVA", "BAJA", "EXCLUIDA", "PENDIENTE_EDAD", "SALIDA_EDAD", "SEGUIMIENTO_CLINICO"}
	wantStates := make(map[string]string)
	for i, state := range states {
		n := (i + 1) * 128
		wantStates[state] = fmt.Sprint(n)
		for j := 0; j < n; j++ {
			if _, err := tx.Exec(`INSERT INTO mujeres (cip,fecha_nacimiento,estado,fecha_estado,fecha_alta)
				VALUES (?,'1980-01-01',?,'2025-01-01','2025-01-01')`, fmt.Sprintf("SYN-%d-%d", i, j), state); err != nil {
				t.Fatal(err)
			}
		}
	}
	var mid int64
	if err := tx.QueryRow(`SELECT MIN(id) FROM mujeres`).Scan(&mid); err != nil {
		t.Fatal(err)
	}
	var wantInv [][6]string
	for year := 2024; year <= 2026; year++ {
		for _, variant := range [][3]string{{"CRIBADO", "CITOLOGIA", "PROF"}, {"CRIBADO", "VPH", "AUTO"}, {"CRIBADO", "VPH", "PROF"}} {
			wantInv = append(wantInv, [6]string{fmt.Sprint(year), variant[0], variant[1], variant[2], "256", "128"})
			for j := 0; j < 256; j++ {
				state := "CERRADA"
				if j%2 == 0 {
					state = "ENVIADA"
				}
				if _, err := tx.Exec(`INSERT INTO invitaciones
					(mujer_id,tipo,ronda,fecha_invitacion,edad,prueba,muestra,motivo,estado)
					VALUES (?,?,1,?,40,?,?,'SYNTHETIC',?)`, mid, variant[0], fmt.Sprintf("%d-01-01", year), variant[1], variant[2], state); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return b, wantStates, wantInv
}

func TestSQLiteWASMRegressionResumen(t *testing.T) {
	b, wantStates, wantInv := sqliteWASMFixture(t)
	// This is the production grouped query, not a substitute SQL implementation.
	// Verify the planner actually selects a sorter for its aggregate ordering.
	rows, err := b.Query(`EXPLAIN QUERY PLAN SELECT estado, COUNT(*) FROM mujeres GROUP BY estado ORDER BY 2 DESC`)
	if err != nil {
		t.Fatal(err)
	}
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail + "\n")
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), "USE TEMP B-TREE FOR ORDER BY") {
		t.Fatalf("aggregate did not exercise sorter: %s", plan.String())
	}
	t.Logf("backend=wasm2go; 2688 women, 2304 invitations; plan:\n%s", plan.String())
	r, err := b.resumen()
	if err != nil {
		t.Fatal(err)
	}
	var want [][2]string
	for _, state := range []string{"SEGUIMIENTO_CLINICO", "SALIDA_EDAD", "PENDIENTE_EDAD", "EXCLUIDA", "BAJA", "ACTIVA"} {
		want = append(want, [2]string{state, wantStates[state]})
	}
	if !reflect.DeepEqual(r.PorEstado, want) || !reflect.DeepEqual(r.Invitaciones, wantInv) || len(r.Cargas) != 0 {
		t.Fatalf("incorrect summary: %+v; want states=%v invitations=%v and no loads", r, want, wantInv)
	}
}

func TestSQLiteWASMRegressionHome(t *testing.T) {
	b, wantStates, _ := sqliteWASMFixture(t)
	sv, err := nuevoServidor(b, false, false)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(sv.rutas())
	defer ts.Close()
	for _, role := range []string{"ADMIN", "COORDINACION"} {
		t.Run(role, func(t *testing.T) {
			if err := b.crearUsuario(role, role, role, "SyntheticInitial2027", "test", "TEST"); err != nil {
				t.Fatal(err)
			}
			n := nuevoNavegador(t, ts.URL)
			n.entrarYCambiar(role, "SyntheticInitial2027", "SyntheticChanged2027")
			resp, err := n.c.Get(ts.URL + "/")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/" {
				t.Fatalf("not authenticated home: status=%d path=%s", resp.StatusCode, resp.Request.URL.Path)
			}
			for _, content := range []string{"<h1>Inicio</h1>", "<h2>Mujeres por estado</h2>", "<h2>Últimos intercambios</h2>", "</html>"} {
				if !strings.Contains(string(body), content) {
					t.Errorf("home missing %q", content)
				}
			}
			for state, count := range wantStates {
				if !strings.Contains(string(body), fmt.Sprintf(`>%s</span></td><td class="num">%s</td>`, state, count)) {
					t.Errorf("home missing %s=%s", state, count)
				}
			}
		})
	}
}
