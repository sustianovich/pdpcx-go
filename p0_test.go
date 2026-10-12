package main

import (
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func prepararP0(t *testing.T, b *BD, dir string) (Programa, int64, int64) {
	t.Helper()
	pg := Programa{b, "prueba-p0", "TEST"}
	ruta := padronCSV(t, dir, [][]string{{"P0A", "100", "1980-01-01"}, {"P0B", "200", "1980-01-01"}})
	if _, err := pg.cargarPadron(ruta, d("2025-01-01"), "TEST"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Exec(`UPDATE mujeres SET fecha_proxima='2025-02-01'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.planificar(d("2025-02-01"), d("2025-02-28"), "P0", "TEST"); err != nil {
		t.Fatal(err)
	}
	var a, z int64
	for cip, destino := range map[string]*int64{"P0A": &a, "P0B": &z} {
		if err := b.QueryRow(`SELECT i.id FROM invitaciones i JOIN mujeres m ON m.id=i.mujer_id WHERE m.cip=?`, cip).Scan(destino); err != nil {
			t.Fatal(err)
		}
	}
	return pg, a, z
}

func txP0(t *testing.T, b *BD, f func(*Tx) error) {
	t.Helper()
	tx, err := b.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := f(tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func fichasP0(t *testing.T, b *BD) []*FichaMujer {
	t.Helper()
	var fichas []*FichaMujer
	for _, cip := range []string{"P0A", "P0B"} {
		id, err := b.buscarMujer(cip)
		if err != nil {
			t.Fatal(err)
		}
		f, err := b.cargarFicha(id)
		if err != nil {
			t.Fatal(err)
		}
		fichas = append(fichas, f)
	}
	return fichas
}

func cerrarInvitacionesP0(t *testing.T, b *BD) {
	t.Helper()
	if _, err := b.Exec(`UPDATE invitaciones SET estado='CERRADA', fecha_cierre='2025-02-02', motivo_cierre='TEST'`); err != nil {
		t.Fatal(err)
	}
}

func TestImportacionIdentidadP0(t *testing.T) {
	for _, tipo := range []string{"PRUEBAS", "COLPOSCOPIAS"} {
		t.Run(tipo, func(t *testing.T) {
			enMotores(t, func(t *testing.T, b *BD, dir string) {
				pg, _, invB := prepararP0(t, b, dir)
				cab := []string{"cip", "invitacion_id", "tipo", "finalidad", "muestra", "fecha_toma", "fecha_resultado", "resultado"}
				fila := []string{"P0A", fmt.Sprint(invB), "VPH", "PRIMARIA", "PROF", "2025-02-10", "2025-02-20", "POS_1618"}
				importar := pg.importarPruebas
				if tipo == "COLPOSCOPIAS" {
					txP0(t, b, func(tx *Tx) error {
						_, err := pg.registrarPrueba(tx, invB, "VPH", "PRIMARIA", "PROF", d("2025-02-10"), d("2025-02-20"), "POS_1618", "TEST")
						return err
					})
					cab = []string{"cip", "invitacion_id", "estado", "fecha", "adecuada", "biopsia", "histologia", "fecha_histologia"}
					fila = []string{"P0A", fmt.Sprint(invB), "REALIZADA", "2025-03-01", "S", "S", "HSIL_CIN3", "2025-03-10"}
					importar = pg.importarColposcopias
				}
				antes := fichasP0(t, b)
				ruta := filepath.Join(dir, "identidad.csv")
				if err := escribirCSV(ruta, cab, [][]string{fila}); err != nil {
					t.Fatal(err)
				}
				rs, err := importar(ruta)
				if err != nil || rs.Aplicadas != 0 || rs.Rechazadas != 1 {
					t.Fatalf("identidad incoherente: %+v, %v", rs, err)
				}
				if !reflect.DeepEqual(antes, fichasP0(t, b)) {
					t.Fatal("la fila rechazada cambió una ficha, sus eventos o decisiones")
				}
				rechazos, err := leerCSVCrudo(rs.Rechazos)
				if err != nil || len(rechazos) != 1 || !strings.Contains(rechazos[0][1], "no pertenece") {
					t.Fatalf("rechazo sin motivo útil: %v, %v", rechazos, err)
				}
				fila[0] = "P0B"
				if err := escribirCSV(ruta, cab, [][]string{fila}); err != nil {
					t.Fatal(err)
				}
				rs, err = importar(ruta)
				if err != nil || rs.Aplicadas != 1 || rs.Rechazadas != 0 {
					t.Fatalf("identidad correcta rechazada: %+v, %v", rs, err)
				}
			})
		})
	}
}

func TestIdentificadoresInvitacionP0(t *testing.T) {
	enMotores(t, func(t *testing.T, b *BD, dir string) {
		_, invA, invB := prepararP0(t, b, dir)
		casos := []struct {
			nombre, cip, inv string
			want             int64
		}{
			{"ambos", "P0A", fmt.Sprint(invA), invA},
			{"solo CIP", "P0A", "", invA},
			{"solo invitación", "", fmt.Sprint(invB), invB},
			{"espacios", " P0A ", " " + fmt.Sprint(invA) + " ", invA},
			{"CIP incorrecto", "P0A", fmt.Sprint(invB), 0},
			{"CIP desconocido", "DESCONOCIDA", fmt.Sprint(invA), 0},
			{"invitación desconocida", "P0A", "999999", 0},
			{"cero", "P0A", "0", 0},
			{"negativo", "P0A", "-1", 0},
			{"texto", "P0A", "abc", 0},
			{"sin identificadores", "", "", 0},
		}
		for _, c := range casos {
			t.Run(c.nombre, func(t *testing.T) {
				tx, err := b.Begin()
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				got, err := invitacionDe(tx, filaCSV{"cip": c.cip, "invitacion_id": c.inv}, d("2025-02-10"))
				if c.want == 0 {
					if err == nil {
						t.Fatalf("identificadores inválidos aceptados: %d", got)
					}
				} else if err != nil || got != c.want {
					t.Fatalf("invitación %d, %v; esperada %d", got, err, c.want)
				}
			})
		}
	})
}

func TestCIPFusionadoP0(t *testing.T) {
	enMotores(t, func(t *testing.T, b *BD, dir string) {
		pg, invA, invB := prepararP0(t, b, dir)
		txP0(t, b, func(tx *Tx) error {
			_, err := pg.registrarPrueba(tx, invA, "VPH", "PRIMARIA", "PROF", d("2025-02-10"), d("2025-02-20"), "NEGATIVO", "TEST")
			return err
		})
		if _, err := pg.fusionar("P0A", "P0B", "duplicado"); err != nil {
			t.Fatal(err)
		}
		ruta := padronCSV(t, dir, [][]string{{"P0C", "300", "1980-01-01"}})
		if _, err := pg.cargarPadron(ruta, d("2025-01-01"), "TEST"); err != nil {
			t.Fatal(err)
		}
		if _, err := pg.fusionar("P0C", "P0A", "segunda fusión"); err != nil {
			t.Fatal(err)
		}
		var idC int64
		if err := b.QueryRow(`SELECT id FROM mujeres WHERE cip='P0C'`).Scan(&idC); err != nil {
			t.Fatal(err)
		}
		txP0(t, b, func(tx *Tx) error {
			for _, inv := range []string{"", fmt.Sprint(invB)} {
				got, err := invitacionDe(tx, filaCSV{"cip": "P0B", "invitacion_id": inv}, d("2025-02-10"))
				if err != nil || got != invB {
					return fmt.Errorf("alias fusionado: invitación %d, %v", got, err)
				}
			}
			mid, err := mujerPorCIP(tx, "P0B")
			if err != nil || mid != idC {
				return fmt.Errorf("ficha principal incorrecta: %d, %v", mid, err)
			}
			_, err = pg.registrarPrueba(tx, invB, "VPH", "PRIMARIA", "PROF", d("2025-02-10"), d("2025-02-20"), "POS_1618", "TEST")
			return err
		})
		if cuenta(t, b, `SELECT COUNT(*) FROM pruebas WHERE invitacion_id=? AND mujer_id=?`, invB, idC) != 1 {
			t.Fatal("la prueba del CIP absorbido no llegó a la ficha principal")
		}
		if _, err := b.Exec(`UPDATE mujeres SET fusionada_en=id WHERE cip='P0B'`); err != nil {
			t.Fatal(err)
		}
		tx, err := b.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := invitacionDe(tx, filaCSV{"cip": "P0B", "invitacion_id": fmt.Sprint(invB)}, d("2025-02-10")); err == nil || !strings.Contains(err.Error(), "circular") {
			t.Fatalf("cadena circular no rechazada: %v", err)
		}
	})
}

func TestWebRollbackP0(t *testing.T) {
	for _, escritura := range []bool{false, true} {
		t.Run(fmt.Sprintf("escritura_%v", escritura), func(t *testing.T) {
			enMotores(t, func(t *testing.T, b *BD, dir string) {
				_, invA, _ := prepararP0(t, b, dir)
				antes := fichasP0(t, b)
				id := antes[0].ID
				sv, err := nuevoServidor(b, false, false)
				if err != nil {
					t.Fatal(err)
				}
				r := httptest.NewRequest("POST", fmt.Sprintf("/mujer/%d/prueba", id), nil)
				r.SetPathValue("id", fmt.Sprint(id))
				w := httptest.NewRecorder()
				s := &Sesion{Usuario: &Usuario{Login: "prueba-p0", Rol: "AP"}}
				activa := make(chan *Tx, 1)
				terminada := make(chan struct{})
				go func() {
					defer close(terminada)
					sv.enTx(w, r, s, "REGISTRO_PRUEBA", "Prueba registrada.", func(tx *Tx, pg Programa) (string, error) {
						activa <- tx
						if escritura {
							if _, err := pg.registrarPrueba(tx, invA, "VPH", "PRIMARIA", "PROF", d("2025-02-10"), d("2025-02-20"), "NEGATIVO", "TEST"); err != nil {
								return "", err
							}
						}
						return "", fmt.Errorf("entrada inválida de prueba")
					})
				}()
				var tx *Tx
				select {
				case tx = <-activa:
				case <-time.After(3 * time.Second):
					t.Fatal("no se pudo iniciar la acción")
				}
				select {
				case <-terminada:
				case <-time.After(3 * time.Second):
					// Liberar la transacción también si esta regresión reaparece.
					tx.Rollback()
					<-terminada
					t.Fatal("la respuesta de error bloqueó la conexión")
				}
				if !strings.Contains(w.Body.String(), "entrada inválida de prueba") || strings.Contains(w.Body.String(), "Prueba registrada.") {
					t.Fatal("el formulario no mostró el error esperado")
				}
				if !reflect.DeepEqual(antes, fichasP0(t, b)) {
					t.Fatal("el rollback no deshizo todos los cambios clínicos")
				}
				if cuenta(t, b, `SELECT COUNT(*) FROM auditoria WHERE accion='REGISTRO_PRUEBA_ERROR'`) != 1 {
					t.Fatal("no se pudo auditar el error tras el rollback")
				}
				get := httptest.NewRecorder()
				sv.ficha(get, r, s)
				if get.Code != 200 || !strings.Contains(get.Body.String(), "P0A") {
					t.Fatal("no se pudo leer la ficha después del error")
				}
			})
		})
	}
}

func TestFusionSeguimientoP0(t *testing.T) {
	enMotores(t, func(t *testing.T, b *BD, dir string) {
		pg, invA, invB := prepararP0(t, b, dir)
		txP0(t, b, func(tx *Tx) error {
			if _, err := pg.registrarPrueba(tx, invB, "VPH", "PRIMARIA", "PROF", d("2025-02-10"), d("2025-02-20"), "POS_1618", "TEST"); err != nil {
				return err
			}
			return pg.registrarColposcopia(tx, invB, "REALIZADA", d("2025-03-01"), true, true, "HSIL_CIN3", d("2025-03-10"), "TEST")
		})
		antes := fichasP0(t, b)
		if _, err := pg.fusionar("P0A", "P0B", "duplicado"); err == nil || !strings.Contains(err.Error(), "episodio abierto") {
			t.Fatalf("seguimiento y episodio abierto no enviados a revisión: %v", err)
		}
		if !reflect.DeepEqual(antes, fichasP0(t, b)) || cuenta(t, b, `SELECT COUNT(*) FROM fusiones`) != 0 {
			t.Fatal("la fusión rechazada cambió los registros")
		}
		txP0(t, b, func(tx *Tx) error {
			_, err := pg.registrarPrueba(tx, invA, "VPH", "PRIMARIA", "PROF", d("2025-02-10"), d("2025-02-20"), "NEGATIVO", "TEST")
			return err
		})
		if _, err := pg.fusionar("P0A", "P0B", "episodio revisado"); err != nil {
			t.Fatal(err)
		}
		f := fichasP0(t, b)[0]
		if f.Estado != "SEGUIMIENTO_CLINICO" || f.FechaEstado != "2025-03-10" || f.Proxima != "" || f.MotivoProx != "" {
			t.Fatalf("seguimiento clínico perdido: %+v", f)
		}
		if cuenta(t, b, `SELECT COUNT(*) FROM mujeres WHERE cip='P0A' AND episodio_control IS NOT NULL`) != 0 {
			t.Fatal("quedó un control de cribado en la ficha en seguimiento")
		}
		if cuenta(t, b, `SELECT COUNT(*) FROM colposcopias WHERE mujer_id=? AND histologia='HSIL_CIN3'`, f.ID) != 1 {
			t.Fatal("no se trasladó la evidencia histológica")
		}
		n := cuenta(t, b, `SELECT COUNT(*) FROM invitaciones`)
		if _, err := pg.planificar(d("2025-04-01"), d("2030-12-31"), "TRAS_FUSION", "TEST"); err != nil {
			t.Fatal(err)
		}
		if cuenta(t, b, `SELECT COUNT(*) FROM invitaciones`) != n {
			t.Fatal("se invitó a cribado a la mujer en seguimiento clínico")
		}
	})
}

func TestFusionEstadosP0(t *testing.T) {
	casos := []struct {
		nombre, p, d, final string
	}{
		{"seguimiento principal", "SEGUIMIENTO_CLINICO", "ACTIVA", "SEGUIMIENTO_CLINICO"},
		{"exclusión duplicada", "ACTIVA", "EXCLUIDA", "EXCLUIDA"},
		{"baja principal", "BAJA", "ACTIVA", "BAJA"},
		{"edad duplicada", "ACTIVA", "SALIDA_EDAD", "SALIDA_EDAD"},
		{"pendiente principal", "PENDIENTE_EDAD", "ACTIVA", "PENDIENTE_EDAD"},
		{"restricciones distintas", "SEGUIMIENTO_CLINICO", "EXCLUIDA", ""},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			enMotores(t, func(t *testing.T, b *BD, dir string) {
				pg, _, _ := prepararP0(t, b, dir)
				cerrarInvitacionesP0(t, b)
				for cip, estado := range map[string]string{"P0A": c.p, "P0B": c.d} {
					if _, err := b.Exec(`UPDATE mujeres SET estado=? WHERE cip=?`, estado, cip); err != nil {
						t.Fatal(err)
					}
				}
				antes := fichasP0(t, b)
				_, err := pg.fusionar("P0A", "P0B", "duplicado")
				if c.final == "" {
					if err == nil || !reflect.DeepEqual(antes, fichasP0(t, b)) {
						t.Fatal("la situación incompatible no se rechazó sin cambios")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				f := fichasP0(t, b)[0]
				if f.Estado != c.final || f.Proxima != "" || f.MotivoProx != "" {
					t.Fatalf("estado restrictivo o cita incorrectos: %+v", f)
				}
			})
		})
	}
}

func TestFusionControlP0(t *testing.T) {
	enMotores(t, func(t *testing.T, b *BD, dir string) {
		pg, invA, invB := prepararP0(t, b, dir)
		txP0(t, b, func(tx *Tx) error {
			if _, err := pg.registrarPrueba(tx, invA, "VPH", "PRIMARIA", "PROF", d("2025-02-10"), d("2025-02-20"), "NEGATIVO", "TEST"); err != nil {
				return err
			}
			if _, err := pg.registrarPrueba(tx, invB, "VPH", "PRIMARIA", "PROF", d("2025-02-10"), d("2025-02-20"), "POS_OTROS", "TEST"); err != nil {
				return err
			}
			_, err := pg.registrarPrueba(tx, invB, "CITOLOGIA", "TRIAJE", "PROF", d("2025-02-10"), d("2025-02-25"), "NEGATIVA", "TEST")
			return err
		})
		if _, err := b.Exec(`UPDATE mujeres SET fecha_proxima='2026-01-01' WHERE cip='P0A'`); err != nil {
			t.Fatal(err)
		}
		if _, err := pg.fusionar("P0A", "P0B", "duplicado"); err != nil {
			t.Fatal(err)
		}
		f := fichasP0(t, b)[0]
		if f.Proxima != "2026-02-25" || f.MotivoProx != "CONTROL" {
			t.Fatalf("el control fue sustituido por rutina: %+v", f)
		}
		if _, err := pg.planificar(d("2026-02-25"), d("2026-02-25"), "CONTROL_FUSION", "TEST"); err != nil {
			t.Fatal(err)
		}
		if cuenta(t, b, `SELECT COUNT(*) FROM invitaciones WHERE mujer_id=? AND tipo='CONTROL' AND episodio_origen=? AND lote='CONTROL_FUSION'`, f.ID, invB) != 1 {
			t.Fatal("el control perdió su episodio de origen")
		}
	})
}

func TestFusionConflictosP0(t *testing.T) {
	casos := []struct {
		nombre, motivo string
		preparar       func(*testing.T, *BD, int64, int64)
	}{
		{"episodios abiertos", "más de un episodio abierto", func(t *testing.T, b *BD, a, z int64) {}},
		{"nacimiento distinto", "fechas de nacimiento distintas", func(t *testing.T, b *BD, a, z int64) {
			if _, err := b.Exec(`UPDATE mujeres SET fecha_nacimiento='1981-01-01' WHERE cip='P0B'`); err != nil {
				t.Fatal(err)
			}
		}},
		{"controles distintos", "controles de episodios distintos", func(t *testing.T, b *BD, a, z int64) {
			cerrarInvitacionesP0(t, b)
			for cip, inv := range map[string]int64{"P0A": a, "P0B": z} {
				if _, err := b.Exec(`UPDATE mujeres SET motivo_proxima='CONTROL', episodio_control=? WHERE cip=?`, inv, cip); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{"exclusión sin resolver", "exclusión definitiva sin resolver", func(t *testing.T, b *BD, a, z int64) {
			cerrarInvitacionesP0(t, b)
			if _, err := b.Exec(`INSERT INTO exclusiones(mujer_id,tipo,motivo,fecha_inicio,origen)
			                    SELECT id,'DEFINITIVA','OTRO','2025-01-01','TEST' FROM mujeres WHERE cip='P0B'`); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			enMotores(t, func(t *testing.T, b *BD, dir string) {
				pg, a, z := prepararP0(t, b, dir)
				c.preparar(t, b, a, z)
				antes := fichasP0(t, b)
				_, err := pg.fusionar("P0A", "P0B", "duplicado")
				if err == nil || !strings.Contains(err.Error(), c.motivo) {
					t.Fatalf("conflicto no enviado a revisión: %v", err)
				}
				if !reflect.DeepEqual(antes, fichasP0(t, b)) || cuenta(t, b, `SELECT COUNT(*) FROM fusiones`) != 0 {
					t.Fatal("la fusión rechazada dejó cambios parciales")
				}
			})
		})
	}
}

func TestFusionExclusionesP0(t *testing.T) {
	enMotores(t, func(t *testing.T, b *BD, dir string) {
		pg, _, _ := prepararP0(t, b, dir)
		cerrarInvitacionesP0(t, b)
		if _, err := b.Exec(`UPDATE mujeres SET fecha_proxima='2025-06-10', motivo_proxima='RUTINA' WHERE cip='P0A'`); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Exec(`UPDATE mujeres SET fecha_proxima='2025-07-01', motivo_proxima='RUTINA' WHERE cip='P0B'`); err != nil {
			t.Fatal(err)
		}
		txP0(t, b, func(tx *Tx) error {
			mid, err := mujerPorCIP(tx, "P0B")
			if err != nil {
				return err
			}
			if err := pg.crearExclusion(tx, mid, "TEMPORAL", "OTRO", d("2025-06-01"), d("2025-06-30"), "TEST"); err != nil {
				return err
			}
			return pg.crearExclusion(tx, mid, "TEMPORAL", "OTRO", d("2025-06-20"), d("2025-07-20"), "TEST")
		})
		if _, err := pg.fusionar("P0A", "P0B", "duplicado"); err != nil {
			t.Fatal(err)
		}
		f := fichasP0(t, b)[0]
		if f.Proxima != "2025-07-21" || len(f.Exclusiones) != 2 {
			t.Fatalf("cita o historial de exclusiones incorrectos: %+v", f)
		}
	})
}

func TestFusionVacunasP0(t *testing.T) {
	for _, conflicto := range []bool{false, true} {
		t.Run(fmt.Sprintf("conflicto_%v", conflicto), func(t *testing.T) {
			enMotores(t, func(t *testing.T, b *BD, dir string) {
				pg, _, _ := prepararP0(t, b, dir)
				cerrarInvitacionesP0(t, b)
				for _, cip := range []string{"P0A", "P0B"} {
					dosis := "1"
					if conflicto && cip == "P0B" {
						dosis = "2"
					}
					ruta := filepath.Join(dir, cip+"-vac.csv")
					if err := escribirCSV(ruta, []string{"cip", "fecha", "dosis", "vacuna"}, [][]string{{cip, "2010-01-01", dosis, "VPH9"}}); err != nil {
						t.Fatal(err)
					}
					if _, err := pg.importarVacunacion(ruta); err != nil {
						t.Fatal(err)
					}
				}
				antes := fichasP0(t, b)
				_, err := pg.fusionar("P0A", "P0B", "duplicado")
				if conflicto {
					if err == nil || !strings.Contains(err.Error(), "vacunas distintas") || !reflect.DeepEqual(antes, fichasP0(t, b)) {
						t.Fatalf("se perdió evidencia de vacunas conflictivas: %v", err)
					}
					if cuenta(t, b, `SELECT COUNT(*) FROM vacunacion_vph`) != 2 || cuenta(t, b, `SELECT COUNT(*) FROM fusiones`) != 0 {
						t.Fatal("la fusión conflictiva dejó cambios parciales")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if cuenta(t, b, `SELECT COUNT(*) FROM vacunacion_vph`) != 1 {
					t.Fatal("no se consolidó la vacuna idéntica")
				}
				var detalle string
				if err := b.QueryRow(`SELECT detalle FROM eventos WHERE tipo='VACUNACION_FUSIONADA'`).Scan(&detalle); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(detalle, "P0A-vac.csv") || !strings.Contains(detalle, "P0B-vac.csv") || !strings.Contains(detalle, "dosis 1 VPH9") {
					t.Fatalf("no se conservaron las dos procedencias: %s", detalle)
				}
			})
		})
	}
}
