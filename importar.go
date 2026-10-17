package main

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Importaciones de la operación real (laboratorio, colposcopia, padrón).
// Cada fila se aplica de forma independiente: una fila errónea no anula las
// demás; los rechazos se escriben en <fichero>.rechazos.csv.

type resumenImport struct {
	Leidas, Aplicadas, Rechazadas int
	Rechazos                      string
}

func (pg Programa) importar(ruta, tipo string, obligatorias []string, aplicar func(tx *Tx, f filaCSV) error) (resumenImport, error) {
	rs := resumenImport{}
	filas, err := leerCSV(ruta, obligatorias...)
	if err != nil {
		return rs, err
	}
	rs.Leidas = len(filas)
	tx, err := pg.Begin()
	if err != nil {
		return rs, err
	}
	defer tx.Rollback()
	var rech [][]string
	for i, f := range filas {
		if _, err := tx.Exec(`SAVEPOINT fila`); err != nil {
			return rs, err
		}
		if err := aplicar(tx, f); err != nil {
			if _, e2 := tx.Exec(`ROLLBACK TO SAVEPOINT fila`); e2 != nil {
				return rs, e2
			}
			rech = append(rech, []string{strconv.Itoa(i + 2), err.Error()})
			rs.Rechazadas++
		} else {
			rs.Aplicadas++
		}
		if _, err := tx.Exec(`RELEASE SAVEPOINT fila`); err != nil {
			return rs, err
		}
	}
	if len(rech) > 0 {
		rs.Rechazos = ruta + ".rechazos.csv"
		if err := escribirCSV(rs.Rechazos, []string{"fila", "motivo"}, rech); err != nil {
			return rs, err
		}
	}
	det := fmt.Sprintf("aplicadas=%d rechazadas=%d", rs.Aplicadas, rs.Rechazadas)
	if err := pg.registrarCarga(tx, tipo, "ENTRADA", ruta, len(filas), det); err != nil {
		return rs, err
	}
	if err := tx.Commit(); err != nil {
		return rs, err
	}
	pg.auditar(pg.Usuario, "", pg.Canal, "IMPORTACION_"+tipo, ruta, det, "")
	return rs, nil
}

func mujerPorCIP(tx *Tx, cip string) (int64, error) {
	cip = strings.TrimSpace(cip)
	if cip == "" {
		return 0, fmt.Errorf("indique cip o invitacion_id")
	}
	var id int64
	err := tx.QueryRow(`SELECT id FROM mujeres WHERE cip=?`, cip).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("CIP %q no existe", cip)
	}
	if err != nil {
		return 0, err
	}
	return mujerCanonica(tx, id)
}

// Los CIP absorbidos siguen identificando a la ficha principal. Una cadena
// incompleta o circular se rechaza antes de asociar información clínica.
func mujerCanonica(tx *Tx, id int64) (int64, error) {
	vistas := map[int64]bool{}
	for {
		if vistas[id] {
			return 0, fmt.Errorf("cadena de fusiones circular en la mujer %d", id)
		}
		vistas[id] = true
		var estado string
		var principal sql.NullInt64
		if err := tx.QueryRow(`SELECT estado, fusionada_en FROM mujeres WHERE id=?`, id).Scan(&estado, &principal); err != nil {
			return 0, fmt.Errorf("resolviendo ficha %d: %w", id, err)
		}
		if estado != "FUSIONADA" {
			return id, nil
		}
		if !principal.Valid || principal.Int64 <= 0 {
			return 0, fmt.Errorf("la ficha fusionada %d no tiene principal válido", id)
		}
		id = principal.Int64
	}
}

// invitacionDe: usa invitacion_id si viene; si no, la invitación abierta de la
// mujer (o la última anterior a la fecha indicada). Si llegan los dos
// identificadores, deben pertenecer a la misma ficha principal.
func invitacionDe(tx *Tx, f filaCSV, fecha time.Time) (int64, error) {
	if s := strings.TrimSpace(f["invitacion_id"]); s != "" {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil || id <= 0 {
			return 0, fmt.Errorf("invitacion_id debe ser un entero positivo: %q", s)
		}
		var propietaria int64
		if err := tx.QueryRow(`SELECT mujer_id FROM invitaciones WHERE id=?`, id).Scan(&propietaria); err != nil {
			return 0, fmt.Errorf("invitación %d: %w", id, err)
		}
		principal, err := mujerCanonica(tx, propietaria)
		if err != nil {
			return 0, err
		}
		if principal != propietaria {
			return 0, fmt.Errorf("invitación %d pendiente de trasladar a la ficha principal", id)
		}
		if cip := strings.TrimSpace(f["cip"]); cip != "" {
			mid, err := mujerPorCIP(tx, cip)
			if err != nil {
				return 0, err
			}
			if mid != propietaria {
				return 0, fmt.Errorf("la invitación %d no pertenece al CIP %q", id, cip)
			}
		}
		return id, nil
	}
	mid, err := mujerPorCIP(tx, f["cip"])
	if err != nil {
		return 0, err
	}
	var id int64
	err = tx.QueryRow(`SELECT id FROM invitaciones WHERE mujer_id=? AND estado<>'CERRADA' ORDER BY fecha_invitacion DESC, id DESC LIMIT 1`, mid).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, err
	}
	err = tx.QueryRow(`SELECT id FROM invitaciones WHERE mujer_id=? AND fecha_invitacion<=? ORDER BY fecha_invitacion DESC, id DESC LIMIT 1`, mid, fs(fecha)).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("CIP %q sin invitación anterior a %s", f["cip"], fs(fecha))
	}
	return id, err
}

func siNo(s string) bool {
	s = strings.ToUpper(strings.TrimSpace(s))
	return s == "S" || s == "SI" || s == "SÍ" || s == "1" || s == "TRUE"
}

func fechaOpc(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return parseFecha(s)
}

// --- pruebas: cip;invitacion_id;tipo;finalidad;muestra;fecha_toma;fecha_resultado;resultado
func (pg Programa) importarPruebas(ruta string) (resumenImport, error) {
	return pg.importar(ruta, "PRUEBAS", []string{"tipo", "finalidad", "muestra", "fecha_toma", "resultado"},
		func(tx *Tx, f filaCSV) error {
			ft, err := parseFecha(f["fecha_toma"])
			if err != nil {
				return err
			}
			fr, err := fechaOpc(f["fecha_resultado"])
			if err != nil {
				return err
			}
			inv, err := invitacionDe(tx, f, ft)
			if err != nil {
				return err
			}
			_, err = pg.registrarPrueba(tx, inv, strings.ToUpper(f["tipo"]), strings.ToUpper(f["finalidad"]),
				strings.ToUpper(f["muestra"]), ft, fr, strings.ToUpper(f["resultado"]), "IMPORT:"+ruta)
			return err
		})
}

// --- colposcopias: cip;invitacion_id;estado;fecha;adecuada;biopsia;histologia;fecha_histologia
func (pg Programa) importarColposcopias(ruta string) (resumenImport, error) {
	return pg.importar(ruta, "COLPOSCOPIAS", []string{"estado", "fecha"},
		func(tx *Tx, f filaCSV) error {
			fc, err := parseFecha(f["fecha"])
			if err != nil {
				return err
			}
			fh, err := fechaOpc(f["fecha_histologia"])
			if err != nil {
				return err
			}
			inv, err := invitacionDe(tx, f, fc)
			if err != nil {
				return err
			}
			return pg.registrarColposcopia(tx, inv, strings.ToUpper(f["estado"]), fc, siNo(f["adecuada"]), siNo(f["biopsia"]),
				strings.ToUpper(f["histologia"]), fh, "IMPORT:"+ruta)
		})
}

// --- invitaciones no válidas (carta devuelta, dirección errónea): invitacion_id;fecha;causa
func (pg Programa) importarNoValidas(ruta string) (resumenImport, error) {
	return pg.importar(ruta, "NO_VALIDAS", []string{"invitacion_id", "fecha", "causa"},
		func(tx *Tx, f filaCSV) error {
			fc, err := parseFecha(f["fecha"])
			if err != nil {
				return err
			}
			id, err := strconv.ParseInt(f["invitacion_id"], 10, 64)
			if err != nil {
				return fmt.Errorf("invitacion_id no numérico")
			}
			return pg.marcarNoValida(tx, id, strings.ToUpper(f["causa"]), fc, "IMPORT:"+ruta)
		})
}

// --- exclusiones: cip;tipo;motivo;fecha_inicio;fecha_fin
func (pg Programa) crearExclusion(tx *Tx, mid int64, tipo, motivo string, ini, fin time.Time, origen string) error {
	if tipo != "DEFINITIVA" && tipo != "TEMPORAL" {
		return fmt.Errorf("tipo de exclusión no válido %q", tipo)
	}
	if tipo == "DEFINITIVA" {
		fin = time.Time{} // Sin fecha de fin, cualquiera que sea el canal de entrada.
	} else {
		if fin.IsZero() {
			return fmt.Errorf("una exclusión TEMPORAL necesita fecha_fin")
		}
		if fin.Before(ini) {
			return fmt.Errorf("el fin es anterior al inicio")
		}
	}
	if _, err := tx.Exec(`INSERT INTO exclusiones(mujer_id,tipo,motivo,fecha_inicio,fecha_fin,origen) VALUES(?,?,?,?,?,?)`,
		mid, tipo, motivo, fs(ini), nullFecha(fin), origen); err != nil {
		return err
	}
	if err := pg.ev(tx, ini, mid, 0, "EXCLUSION_"+tipo, motivo, origen); err != nil {
		return err
	}
	if tipo == "DEFINITIVA" {
		_, err := tx.Exec(`UPDATE mujeres SET estado='EXCLUIDA', fecha_estado=?, fecha_proxima=NULL, motivo_proxima=NULL
		                   WHERE id=? AND estado IN ('ACTIVA','PENDIENTE_EDAD','SEGUIMIENTO_CLINICO')`, fs(ini), mid)
		return err
	}
	// Temporal: si la próxima cita cae dentro, se mueve al día siguiente del fin.
	var fp sql.NullString
	if err := tx.QueryRow(`SELECT fecha_proxima FROM mujeres WHERE id=?`, mid).Scan(&fp); err != nil {
		return err
	}
	if fp.Valid {
		p := mustFecha(fp.String)
		if !p.Before(ini) && !p.After(fin) {
			if _, err := tx.Exec(`UPDATE mujeres SET fecha_proxima=? WHERE id=?`, fs(fin.AddDate(0, 0, 1)), mid); err != nil {
				return err
			}
		}
	}
	return nil
}

func (pg Programa) importarExclusiones(ruta string) (resumenImport, error) {
	return pg.importar(ruta, "EXCLUSIONES", []string{"cip", "tipo", "motivo", "fecha_inicio"},
		func(tx *Tx, f filaCSV) error {
			mid, err := mujerPorCIP(tx, f["cip"])
			if err != nil {
				return err
			}
			ini, err := parseFecha(f["fecha_inicio"])
			if err != nil {
				return err
			}
			fin, err := fechaOpc(f["fecha_fin"])
			if err != nil {
				return err
			}
			return pg.crearExclusion(tx, mid, strings.ToUpper(f["tipo"]), strings.ToUpper(f["motivo"]), ini, fin, "IMPORT:"+ruta)
		})
}

// --- bajas de padrón/TIS (fallecimiento, traslado): cip;fecha;motivo
func (pg Programa) importarBajas(ruta string) (resumenImport, error) {
	return pg.importar(ruta, "BAJAS", []string{"cip", "fecha", "motivo"},
		func(tx *Tx, f filaCSV) error {
			mid, err := mujerPorCIP(tx, f["cip"])
			if err != nil {
				return err
			}
			fc, err := parseFecha(f["fecha"])
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE mujeres SET estado='BAJA', fecha_estado=?, fecha_proxima=NULL, motivo_proxima=NULL WHERE id=?`, fs(fc), mid); err != nil {
				return err
			}
			return pg.ev(tx, fc, mid, 0, "BAJA", strings.ToUpper(f["motivo"]), "IMPORT:"+ruta)
		})
}

// reincorporar devuelve al programa a una mujer en seguimiento clínico o excluida.
func (pg Programa) reincorporar(cip string, f time.Time, motivo string) error {
	tx, err := pg.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	mid, err := mujerPorCIP(tx, cip)
	if err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE mujeres SET estado='ACTIVA', fecha_estado=?, fecha_proxima=?, motivo_proxima='REINCORPORACION'
	                     WHERE id=? AND estado IN ('SEGUIMIENTO_CLINICO','EXCLUIDA')`, fs(f), fs(f), mid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("la mujer %s no está en SEGUIMIENTO_CLINICO ni EXCLUIDA", cip)
	}
	if err := pg.ev(tx, f, mid, 0, "REINCORPORACION", motivo, "MANUAL"); err != nil {
		return err
	}
	return tx.Commit()
}

func avisoRechazos(rs resumenImport) {
	fmt.Printf("Leídas %d · aplicadas %d · rechazadas %d\n", rs.Leidas, rs.Aplicadas, rs.Rechazadas)
	if rs.Rechazos != "" {
		fmt.Fprintf(os.Stderr, "Detalle de rechazos: %s\n", rs.Rechazos)
	}
}
