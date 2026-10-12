package main

import (
	"fmt"
	"time"
)

type resumenPlan struct {
	CerradasSinRespuesta int
	Invitaciones         map[string]int
	AplazadasExclusion   int
	SalenPorEdad         int
}

// planificar genera las invitaciones de las mujeres ACTIVAS cuya próxima cita
// cae en o antes de 'hasta'. Reglas:
//   - edad a la fecha de invitación: 25-29 citología, 30-65 VPH-AR;
//   - nunca se invita a quien ya ha cumplido 66 (pasa a SALIDA_EDAD);
//   - una mujer con una invitación abierta no recibe otra;
//   - exclusión temporal vigente -> se aplaza hasta su fin;
//   - autotoma (solo VPH) según autotoma_inicio / autotoma_estrategia.
func (pg Programa) planificar(desde, hasta time.Time, lote, origen string) (resumenPlan, error) {
	if esLoteMensual(lote) {
		return resumenPlan{}, fmt.Errorf("los nombres AAAA-MM están reservados para Planificación mensual")
	}
	tx, err := pg.Begin()
	if err != nil {
		return resumenPlan{}, err
	}
	defer tx.Rollback()
	if err := bloquearPlanificacion(tx); err != nil {
		return resumenPlan{}, err
	}
	if err := comprobarLoteNuevo(tx, lote); err != nil {
		return resumenPlan{}, err
	}
	rs, err := pg.planificarEnTx(tx, desde, hasta, lote, origen)
	if err != nil {
		return rs, err
	}
	return rs, tx.Commit()
}

// exportarLote saca las invitaciones de un lote para la imprenta / SMS.
var cabLote = []string{"invitacion_id", "cip", "nhc", "zbs", "municipio", "cod_postal",
	"fecha_invitacion", "tipo", "prueba", "muestra", "motivo", "edad"}

func (pg Programa) filasLote(lote string) ([][]string, error) {
	rows, err := pg.Query(`
	    SELECT i.id, m.cip, COALESCE(m.nhc,''), COALESCE(m.zbs,''), COALESCE(m.municipio,''), COALESCE(m.cod_postal,''),
	           i.fecha_invitacion, i.tipo, i.prueba, i.muestra, i.motivo, i.edad
	    FROM invitaciones i JOIN mujeres m ON m.id=i.mujer_id
	    WHERE i.lote=? ORDER BY i.fecha_invitacion, i.id`, lote)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var filas [][]string
	for rows.Next() {
		var id int64
		var cip, nhc, zbs, mun, cp, f, tipo, prueba, muestra, motivo string
		var e int
		if err := rows.Scan(&id, &cip, &nhc, &zbs, &mun, &cp, &f, &tipo, &prueba, &muestra, &motivo, &e); err != nil {
			return nil, err
		}
		filas = append(filas, []string{fmt.Sprint(id), cip, nhc, zbs, mun, cp, f, tipo, prueba, muestra, motivo, fmt.Sprint(e)})
	}
	return filas, rows.Err()
}

// registrarExportacion deja la salida como intercambio y en auditoría.
func (pg Programa) registrarExportacion(lote, destino, ip string, n int) error {
	if err := pg.registrarCarga(pg, "LOTE_INVITACIONES", "SALIDA", destino, n, "lote "+lote); err != nil {
		return err
	}
	pg.auditar(pg.Usuario, "", pg.Canal, "LOTE_EXPORTADO", lote, fmt.Sprintf("%d invitaciones -> %s", n, destino), ip)
	return nil
}

func (pg Programa) exportarLote(lote, ruta string) (int, error) {
	filas, err := pg.filasLote(lote)
	if err != nil {
		return 0, err
	}
	if err := escribirCSV(ruta, cabLote, filas); err != nil {
		return 0, err
	}
	return len(filas), pg.registrarExportacion(lote, ruta, "", len(filas))
}
