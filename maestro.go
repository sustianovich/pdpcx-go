package main

import (
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Registro maestro: vacunación y fusión de duplicados
// ---------------------------------------------------------------------------

// --- vacunación VPH: cip;fecha;dosis;vacuna
func (pg Programa) importarVacunacion(ruta string) (resumenImport, error) {
	return pg.importar(ruta, "VACUNACION", []string{"cip", "fecha"},
		func(tx *Tx, f filaCSV) error {
			mid, err := mujerPorCIP(tx, f["cip"])
			if err != nil {
				return err
			}
			fv, err := parseFecha(f["fecha"])
			if err != nil {
				return err
			}
			var dosis any
			if f["dosis"] != "" {
				n, err := strconv.Atoi(f["dosis"])
				if err != nil {
					return fmt.Errorf("dosis no numérica %q", f["dosis"])
				}
				dosis = n
			}
			res, err := tx.Exec(`INSERT INTO vacunacion_vph(mujer_id,fecha,dosis,vacuna,origen) VALUES(?,?,?,?,?)
			                     ON CONFLICT (mujer_id, fecha) DO NOTHING`, mid, fs(fv), dosis, f["vacuna"], "IMPORT:"+ruta)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				return pg.ev(tx, fv, mid, 0, "VACUNACION_VPH", fmt.Sprintf("dosis %v %s", dosis, f["vacuna"]), "IMPORT:"+ruta)
			}
			return nil
		})
}

// fusionar une dos registros de la misma persona. Todo lo de la duplicada
// (invitaciones, pruebas, colposcopias, exclusiones, eventos, historial,
// vacunas, decisiones) pasa a la principal; la duplicada queda FUSIONADA y
// apuntando a la principal. Las vacunas idénticas se consolidan dejando
// constancia de ambas procedencias; los conflictos impiden la fusión.
func (pg Programa) fusionar(cipPrincipal, cipDuplicada, motivo string) (string, error) {
	if strings.TrimSpace(motivo) == "" {
		return "", fmt.Errorf("indique el motivo de la fusión")
	}
	if cipPrincipal == cipDuplicada {
		return "", fmt.Errorf("los dos CIP son iguales")
	}
	tx, err := pg.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if pg.pg {
		// Bloquear ambas filas en orden estable también protege de fusiones
		// simultáneas que comparten uno de los registros.
		rows, err := tx.Query(`SELECT id FROM mujeres WHERE cip IN (?,?) ORDER BY id FOR UPDATE`, cipPrincipal, cipDuplicada)
		if err != nil {
			return "", err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return "", err
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return "", err
		}
	}
	p, err := cargarPersonaFusion(tx, cipPrincipal)
	if err != nil {
		return "", fmt.Errorf("CIP principal %s: %w", cipPrincipal, err)
	}
	d, err := cargarPersonaFusion(tx, cipDuplicada)
	if err != nil {
		return "", fmt.Errorf("CIP duplicado %s: %w", cipDuplicada, err)
	}
	final, err := pg.reconciliarFusion(tx, p, d)
	if err != nil {
		return "", err
	}
	idP, idD := p.ID, d.ID
	if err := pg.conciliarVacunasFusion(tx, idP, idD); err != nil {
		return "", err
	}
	var det []string
	for _, t := range []string{"invitaciones", "pruebas", "colposcopias", "exclusiones", "eventos", "historial_persona", "vacunacion_vph", "decisiones"} {
		res, err := tx.Exec(`UPDATE `+t+` SET mujer_id=? WHERE mujer_id=?`, idP, idD)
		if err != nil {
			return "", fmt.Errorf("%s: %w", t, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			det = append(det, fmt.Sprintf("%s=%d", t, n))
		}
	}
	f := hoy()
	if _, err := tx.Exec(`UPDATE mujeres SET estado='FUSIONADA', fecha_estado=?, fusionada_en=?, fecha_proxima=NULL, motivo_proxima=NULL, episodio_control=NULL WHERE id=?`, fs(f), idP, idD); err != nil {
		return "", err
	}
	if _, err := tx.Exec(`UPDATE mujeres SET estado=?, fecha_estado=?, fecha_proxima=?, motivo_proxima=?, episodio_control=? WHERE id=?`,
		final.Estado, final.Desde, final.Proxima, final.Motivo, final.Control, idP); err != nil {
		return "", err
	}
	if p.Estado != final.Estado || p.Proxima != final.Proxima || p.Motivo != final.Motivo || p.Control != final.Control || p.Desde != final.Desde {
		detalle := fmt.Sprintf("estado %s -> %s; próxima %q -> %q; motivo %q; control %v", p.Estado, final.Estado, p.Proxima.String, final.Proxima.String, final.Motivo.String, final.Control)
		if err := pg.ev(tx, f, idP, 0, "CONCILIACION_FUSION", detalle, "FUSION"); err != nil {
			return "", err
		}
	}
	if _, err := tx.Exec(`INSERT INTO historial_persona(mujer_id,fecha,campo,anterior,nuevo,origen,usuario) VALUES(?,?,?,?,?,?,?)`,
		idP, fs(f), "FUSION", cipDuplicada, cipPrincipal, "FUSION", pg.Usuario); err != nil {
		return "", err
	}
	det = append(det, "estado="+final.Estado)
	if final.Proxima.Valid {
		det = append(det, "proxima="+final.Proxima.String)
	}
	detalle := strings.Join(det, ", ")
	if _, err := tx.Exec(`INSERT INTO fusiones(principal,duplicada,cip_duplicada,usuario,motivo,detalle) VALUES(?,?,?,?,?,?)`,
		idP, idD, cipDuplicada, pg.Usuario, motivo, detalle); err != nil {
		return "", err
	}
	if err := pg.ev(tx, f, idP, 0, "FUSION_REGISTROS", "absorbe "+cipDuplicada+": "+detalle, "FUSION"); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	pg.auditar(pg.Usuario, "", pg.Canal, "FUSION", cipPrincipal+" <- "+cipDuplicada, motivo+" ("+detalle+")", "")
	return detalle, nil
}

// ---------------------------------------------------------------------------
// Lista viva: tareas pendientes por servicio, con antigüedad y plazo
// ---------------------------------------------------------------------------

var servicios = []string{"ADMISION", "AP", "LABORATORIO", "GINECOLOGIA", "COORDINACION"}

type Tarea struct {
	Servicio, Tarea, CIP, NHC, Detalle string
	MujerID, InvID                     int64
	Desde                              time.Time
	Dias, Plazo                        int
	Vencida                            bool
}

func (b *BD) pendientes(servicio string, ref time.Time) ([]Tarea, error) {
	var out []Tarea
	type def struct {
		servicio, tarea, plazoPar string
		umbral                    int // días desde los que aparece (0 = siempre)
		sql                       string
	}
	// Cada consulta devuelve: mujer_id, cip, nhc, invitacion_id, fecha_desde, detalle
	ultRes := `COALESCE((SELECT MAX(COALESCE(p.fecha_resultado,p.fecha_toma)) FROM pruebas p WHERE p.invitacion_id=i.id), i.fecha_invitacion)`
	defs := []def{
		{"ADMISION", "Enviar recordatorio (sin respuesta)", "plazo_recordatorio_dias", b.pInt("plazo_recordatorio_dias"),
			`SELECT m.id, m.cip, COALESCE(m.nhc,''), i.id, i.fecha_invitacion, i.prueba || ' ' || i.muestra
			 FROM invitaciones i JOIN mujeres m ON m.id=i.mujer_id WHERE i.estado='ENVIADA'`},
		{"ADMISION", "Revisar dirección o teléfono (invitación no válida)", "plazo_ap_dias", 0,
			`SELECT m.id, m.cip, COALESCE(m.nhc,''), i.id, i.fecha_cierre, COALESCE(i.causa_no_valida,'')
			 FROM invitaciones i JOIN mujeres m ON m.id=i.mujer_id
			 WHERE i.motivo_cierre='NO_VALIDA' AND m.estado='ACTIVA'
			   AND NOT EXISTS (SELECT 1 FROM invitaciones j WHERE j.mujer_id=i.mujer_id AND j.fecha_invitacion > i.fecha_invitacion)
			   AND NOT EXISTS (SELECT 1 FROM historial_persona h WHERE h.mujer_id=m.id AND h.fecha >= i.fecha_cierre)`},
		{"AP", "Citar toma por profesional para citología de triaje (autotoma VPH+)", "plazo_ap_dias", 0,
			`SELECT m.id, m.cip, COALESCE(m.nhc,''), i.id, ` + ultRes + `, 'VPH otros AR en autotoma'
			 FROM invitaciones i JOIN mujeres m ON m.id=i.mujer_id WHERE i.estado='TRIAJE_PENDIENTE' AND i.muestra='AUTO'`},
		{"AP", "Citar repetición de muestra no válida", "plazo_ap_dias", 0,
			`SELECT m.id, m.cip, COALESCE(m.nhc,''), i.id, ` + ultRes + `, i.prueba
			 FROM invitaciones i JOIN mujeres m ON m.id=i.mujer_id WHERE i.estado='REPETIR_MUESTRA'`},
		{"LABORATORIO", "Triaje réflex pendiente", "plazo_laboratorio_dias", 0,
			`SELECT m.id, m.cip, COALESCE(m.nhc,''), i.id, ` + ultRes + `, CASE WHEN i.prueba='VPH' THEN 'citología de triaje' ELSE 'VPH de triaje' END
			 FROM invitaciones i JOIN mujeres m ON m.id=i.mujer_id WHERE i.estado='TRIAJE_PENDIENTE' AND i.muestra='PROF'`},
		{"GINECOLOGIA", "Realizar colposcopia", "plazo_colposcopia_dias", 0,
			`SELECT m.id, m.cip, COALESCE(m.nhc,''), c.invitacion_id, c.fecha_derivacion, c.motivo_derivacion
			 FROM colposcopias c JOIN mujeres m ON m.id=c.mujer_id
			 JOIN invitaciones i ON i.id=c.invitacion_id
			 WHERE c.estado='DERIVADA' AND i.estado='DERIVADA_COLPOSCOPIA'`},
		{"GINECOLOGIA", "Registrar resultado histológico", "plazo_histologia_dias", 0,
			`SELECT m.id, m.cip, COALESCE(m.nhc,''), c.invitacion_id, c.fecha_realizacion, 'biopsia sin resultado'
			 FROM colposcopias c JOIN mujeres m ON m.id=c.mujer_id
			 WHERE c.estado='REALIZADA' AND c.biopsia=1 AND c.histologia IS NULL`},
		{"COORDINACION", "Valorar reincorporación tras seguimiento clínico", "", b.pInt("meses_valorar_reincorporacion") * 30,
			`SELECT m.id, m.cip, COALESCE(m.nhc,''), 0, m.fecha_estado, 'en seguimiento clínico'
			 FROM mujeres m WHERE m.estado='SEGUIMIENTO_CLINICO'`},
	}
	for _, d := range defs {
		if servicio != "" && servicio != "COORDINACION" && d.servicio != servicio {
			continue
		}
		plazo := d.umbral
		if d.plazoPar != "" {
			plazo = b.pInt(d.plazoPar)
		}
		rows, err := b.Query(d.sql)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", d.tarea, err)
		}
		for rows.Next() {
			var t Tarea
			var desde sql.NullString
			if err := rows.Scan(&t.MujerID, &t.CIP, &t.NHC, &t.InvID, &desde, &t.Detalle); err != nil {
				rows.Close()
				return nil, err
			}
			if !desde.Valid {
				continue
			}
			t.Desde = mustFecha(desde.String)
			if t.Desde.After(ref) {
				continue // aún no ha ocurrido a la fecha de referencia
			}
			t.Dias = int(ref.Sub(t.Desde).Hours() / 24)
			if d.umbral > 0 && t.Dias < d.umbral {
				continue
			}
			t.Servicio, t.Tarea, t.Plazo = d.servicio, d.tarea, plazo
			t.Vencida = t.Dias > plazo
			if d.servicio == "ADMISION" && d.umbral > 0 {
				t.Vencida = t.Dias > b.pInt("plazo_respuesta_dias")-30
			}
			out = append(out, t)
		}
		rows.Close()
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Vencida != out[j].Vencida {
			return out[i].Vencida
		}
		return out[i].Dias > out[j].Dias
	})
	return out, nil
}
