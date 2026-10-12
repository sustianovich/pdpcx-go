package main

import (
	"database/sql"
	"fmt"
	"time"
)

// Los datos que determinan si una fusión puede conservar la elegibilidad
// y la cita de forma inequívoca. Los conflictos se revisan antes de escribir.
type personaFusion struct {
	ID                        int64
	Nacimiento, Estado, Desde string
	Proxima, Motivo           sql.NullString
	Control                   sql.NullInt64
	Abiertas                  int
}

func cargarPersonaFusion(tx *Tx, cip string) (personaFusion, error) {
	var p personaFusion
	err := tx.QueryRow(`SELECT m.id, m.fecha_nacimiento, m.estado, m.fecha_estado,
	                          m.fecha_proxima, m.motivo_proxima, m.episodio_control,
	                          (SELECT COUNT(*) FROM invitaciones i WHERE i.mujer_id=m.id AND i.estado<>'CERRADA')
	                   FROM mujeres m WHERE m.cip=?`, cip).
		Scan(&p.ID, &p.Nacimiento, &p.Estado, &p.Desde, &p.Proxima, &p.Motivo, &p.Control, &p.Abiertas)
	return p, err
}

func (pg Programa) reconciliarFusion(tx *Tx, p, d personaFusion) (personaFusion, error) {
	if p.Nacimiento != d.Nacimiento {
		return p, fmt.Errorf("fechas de nacimiento distintas: revise los datos antes de fusionar")
	}
	if p.Estado == "FUSIONADA" || d.Estado == "FUSIONADA" {
		return p, fmt.Errorf("uno de los registros ya está fusionado")
	}
	if p.Abiertas+d.Abiertas > 1 {
		return p, fmt.Errorf("hay más de un episodio abierto: revise y cierre los incompatibles antes de fusionar")
	}
	final := p
	if p.Estado != d.Estado {
		switch {
		case p.Estado == "ACTIVA":
			final.Estado, final.Desde = d.Estado, d.Desde
		case d.Estado == "ACTIVA":
			// La fusión conserva el estado restrictivo de la principal.
		default:
			return p, fmt.Errorf("estados incompatibles %s y %s: revise la situación antes de fusionar", p.Estado, d.Estado)
		}
	} else if final.Estado != "ACTIVA" && d.Desde < p.Desde {
		final.Desde = d.Desde // no retrasar el inicio del seguimiento pendiente
	}
	if final.Estado != "ACTIVA" {
		if p.Abiertas+d.Abiertas > 0 {
			return p, fmt.Errorf("el estado %s es incompatible con un episodio abierto: revíselo antes de fusionar", final.Estado)
		}
		final.Proxima, final.Motivo, final.Control = sql.NullString{}, sql.NullString{}, sql.NullInt64{}
		return final, nil
	}

	// Un control pendiente no puede sustituirse por una cita rutinaria ni
	// resolverse contra otro episodio de control sin revisión.
	controlP := p.Proxima.Valid && p.Motivo.String == "CONTROL"
	controlD := d.Proxima.Valid && d.Motivo.String == "CONTROL"
	if controlP && controlD && p.Control != d.Control {
		return p, fmt.Errorf("controles de episodios distintos: revise el seguimiento antes de fusionar")
	}
	if p.Abiertas > 0 && controlD || d.Abiertas > 0 && controlP {
		return p, fmt.Errorf("un registro tiene un episodio abierto y el otro un control pendiente: revise el seguimiento antes de fusionar")
	}
	cita := p
	switch {
	case d.Abiertas > 0:
		cita = d
	case p.Abiertas > 0:
	case controlD && !controlP:
		cita = d
	case controlP && !controlD:
	case d.Proxima.Valid && (!p.Proxima.Valid || d.Proxima.String < p.Proxima.String):
		cita = d
	}
	final.Proxima, final.Motivo, final.Control = cita.Proxima, cita.Motivo, cita.Control
	if !final.Proxima.Valid {
		final.Motivo, final.Control = sql.NullString{}, sql.NullInt64{}
	} else if final.Motivo.String == "CONTROL" {
		if !final.Control.Valid || final.Control.Int64 <= 0 {
			return p, fmt.Errorf("control pendiente sin episodio de origen: revíselo antes de fusionar")
		}
		var mid int64
		if err := tx.QueryRow(`SELECT mujer_id FROM invitaciones WHERE id=?`, final.Control.Int64).Scan(&mid); err != nil {
			return p, fmt.Errorf("comprobando origen del control: %w", err)
		}
		if mid != p.ID && mid != d.ID {
			return p, fmt.Errorf("el control pertenece a otra mujer: revíselo antes de fusionar")
		}
	} else {
		final.Control = sql.NullInt64{}
	}

	var definitivas int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM exclusiones WHERE mujer_id IN (?,?) AND tipo='DEFINITIVA'
	                      AND (fecha_fin IS NULL OR fecha_fin>=?)`, p.ID, d.ID, fs(hoy())).Scan(&definitivas); err != nil {
		return p, err
	}
	if definitivas > 0 {
		return p, fmt.Errorf("hay una exclusión definitiva sin resolver en una ficha ACTIVA: revísela antes de fusionar")
	}
	if !final.Proxima.Valid {
		return final, nil
	}
	fecha, err := parseFecha(final.Proxima.String)
	if err != nil {
		return p, err
	}
	rows, err := tx.Query(`SELECT fecha_inicio, fecha_fin FROM exclusiones
	                      WHERE mujer_id IN (?,?) AND tipo='TEMPORAL' ORDER BY fecha_inicio, fecha_fin`, p.ID, d.ID)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var ini string
		var fin sql.NullString
		if err := rows.Scan(&ini, &fin); err != nil {
			return p, err
		}
		a, err := parseFecha(ini)
		if err != nil {
			return p, err
		}
		z, err := parseFecha(fin.String)
		if !fin.Valid || err != nil || z.Before(a) {
			return p, fmt.Errorf("exclusión temporal con fechas inválidas: revísela antes de fusionar")
		}
		if !fecha.Before(a) && !fecha.After(z) {
			fecha = z.AddDate(0, 0, 1)
		}
	}
	if err := rows.Err(); err != nil {
		return p, err
	}
	final.Proxima.String = fs(fecha)
	nac, err := parseFecha(p.Nacimiento)
	if err != nil {
		return p, err
	}
	salida := cumple(nac, pg.pInt("edad_vph_max")+1)
	if !fecha.Before(salida) {
		if p.Abiertas+d.Abiertas > 0 {
			return p, fmt.Errorf("cita fuera de edad con episodio abierto: revíselo antes de fusionar")
		}
		final.Estado, final.Desde = "SALIDA_EDAD", fs(salida)
		final.Proxima, final.Motivo, final.Control = sql.NullString{}, sql.NullString{}, sql.NullInt64{}
	}
	return final, nil
}

// La clave única de vacunas es mujer/fecha. Las discrepancias no se borran.
// Si la evidencia clínica coincide, se conserva la procedencia de las dos
// filas en la traza antes de consolidarlas dentro de la misma transacción.
func (pg Programa) conciliarVacunasFusion(tx *Tx, principal, duplicada int64) error {
	rows, err := tx.Query(`SELECT d.id, p.id, d.fecha, d.dosis, p.dosis, COALESCE(d.vacuna,''), COALESCE(p.vacuna,''),
	                             COALESCE(d.origen,''), COALESCE(p.origen,'')
	                      FROM vacunacion_vph d JOIN vacunacion_vph p ON p.fecha=d.fecha
	                      WHERE d.mujer_id=? AND p.mujer_id=? ORDER BY d.fecha`, duplicada, principal)
	if err != nil {
		return err
	}
	type coincidente struct {
		id      int64
		fecha   time.Time
		detalle string
	}
	var iguales []coincidente
	for rows.Next() {
		var idD, idP int64
		var fecha, vacD, vacP, origenD, origenP string
		var dosisD, dosisP sql.NullInt64
		if err := rows.Scan(&idD, &idP, &fecha, &dosisD, &dosisP, &vacD, &vacP, &origenD, &origenP); err != nil {
			rows.Close()
			return err
		}
		if dosisD != dosisP || vacD != vacP {
			rows.Close()
			return fmt.Errorf("vacunas distintas en la fecha %s: revise las dos evidencias antes de fusionar", fecha)
		}
		f, err := parseFecha(fecha)
		if err != nil {
			rows.Close()
			return err
		}
		dosis := "sin dosis registrada"
		if dosisD.Valid {
			dosis = fmt.Sprintf("dosis %d", dosisD.Int64)
		}
		iguales = append(iguales, coincidente{idD, f,
			fmt.Sprintf("vacuna %d consolidada en %d: %s %s; origen duplicada=%q; origen principal=%q", idD, idP, dosis, vacD, origenD, origenP)})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, v := range iguales {
		if err := pg.ev(tx, v.fecha, principal, 0, "VACUNACION_FUSIONADA", v.detalle, "FUSION"); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM vacunacion_vph WHERE id=?`, v.id); err != nil {
			return err
		}
	}
	return nil
}
