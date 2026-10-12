package main

import (
	"database/sql"
	"fmt"
	"time"
)

var nombresMes = []string{"Enero", "Febrero", "Marzo", "Abril", "Mayo", "Junio", "Julio", "Agosto", "Septiembre", "Octubre", "Noviembre", "Diciembre"}

type conteoMes struct {
	Citologia, VPH, Controles, Reinvitaciones, Salidas int
}

func (c conteoMes) Total() int { return c.Citologia + c.VPH + c.Controles + c.Reinvitaciones }
func (c *conteoMes) contar(tipo, prueba, motivo string, n int) {
	switch {
	case tipo == "CONTROL":
		c.Controles += n
	case motivo == "NO_RESPONDE" || motivo == "NO_VALIDA" || motivo == "SIN_RESULTADO" || motivo == "NO_ACUDE_COLPOSCOPIA":
		c.Reinvitaciones += n
	case prueba == "CITOLOGIA":
		c.Citologia += n
	case prueba == "VPH":
		c.VPH += n
	}
}

type mesPlan struct {
	conteoMes
	Numero                               int
	Nombre, Lote, Estado, Fecha, Usuario string
	Generado, PuedeGenerar               bool
}

type calendarioPlan struct {
	Anio, Anterior, Siguiente, Atrasadas      int
	Proximo                                   string
	Meses                                     []mesPlan
	CitoMin, CitoMax, VPHMin, VPHMax          int
	IntervaloCito, IntervaloVPH, ControlMeses string
}

func limitesMes(anio, mes int) (time.Time, time.Time, error) {
	if anio < 1900 || anio > 9998 || mes < 1 || mes > 12 {
		return time.Time{}, time.Time{}, fmt.Errorf("año o mes no válido (año 1900–9998, mes 1–12)")
	}
	desde := time.Date(anio, time.Month(mes), 1, 0, 0, 0, 0, time.Local)
	return desde, desde.AddDate(0, 1, -1), nil
}

func esLoteMensual(lote string) bool {
	_, err := time.Parse("2006-01", lote)
	return len(lote) == 7 && err == nil
}

func siguienteMes(tx *Tx, anio int) (string, error) {
	var ultimo sql.NullString
	if err := tx.QueryRow(`SELECT MAX(periodo) FROM planificacion_mensual`).Scan(&ultimo); err != nil {
		return "", err
	}
	if !ultimo.Valid {
		return fmt.Sprintf("%04d-01", anio), nil
	}
	f, err := time.Parse("2006-01", ultimo.String)
	if err != nil {
		return "", err
	}
	return f.AddDate(0, 1, 0).Format("2006-01"), nil
}

func (pg Programa) generarMes(anio, mes int, origen string) (resumenPlan, error) {
	desde, hasta, err := limitesMes(anio, mes)
	if err != nil {
		return resumenPlan{}, err
	}
	lote := desde.Format("2006-01")
	tx, err := pg.Begin()
	if err != nil {
		return resumenPlan{}, err
	}
	defer tx.Rollback()
	if err := bloquearPlanificacion(tx); err != nil {
		return resumenPlan{}, err
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM planificacion_mensual WHERE periodo=?`, lote).Scan(&n); err != nil {
		return resumenPlan{}, err
	}
	if n > 0 {
		return resumenPlan{}, fmt.Errorf("el mes %s ya está generado", lote)
	}
	siguiente, err := siguienteMes(tx, anio)
	if err != nil {
		return resumenPlan{}, err
	}
	if lote != siguiente {
		return resumenPlan{}, fmt.Errorf("no se pueden saltar meses: genere primero %s", siguiente)
	}
	if err := comprobarLoteNuevo(tx, lote); err != nil {
		return resumenPlan{}, err
	}
	rs, err := pg.planificarEnTx(tx, desde, hasta, lote, origen)
	if err != nil {
		return rs, err
	}
	if _, err := tx.Exec(`INSERT INTO planificacion_mensual(periodo,lote,usuario,cerradas,aplazadas,salidas) VALUES(?,?,?,?,?,?)`,
		lote, lote, pg.Usuario, rs.CerradasSinRespuesta, rs.AplazadasExclusion, rs.SalenPorEdad); err != nil {
		return rs, err
	}
	return rs, tx.Commit()
}

func (pg Programa) calendarioMensual(anio int, fechaActual time.Time) (calendarioPlan, error) {
	cal := calendarioPlan{Anio: anio, Anterior: anio - 1, Siguiente: anio + 1}
	if _, _, err := limitesMes(anio, 1); err != nil {
		return cal, err
	}
	r, err := pg.reglasPlanificacion()
	if err != nil {
		return cal, err
	}
	cal.CitoMin, cal.CitoMax, cal.VPHMin, cal.VPHMax = r.CitoMin, r.CitoMax, r.VPHMin, r.VPHMax
	cal.IntervaloCito, cal.IntervaloVPH, cal.ControlMeses = pg.getPar("intervalo_cito_meses"), pg.getPar("intervalo_vph_meses"), pg.getPar("control_meses")
	tx, err := pg.Begin()
	if err != nil {
		return cal, err
	}
	defer tx.Rollback()
	cal.Proximo, err = siguienteMes(tx, anio)
	if err != nil {
		return cal, err
	}
	ds, err := leerDatosPlan(tx)
	if err != nil {
		return cal, err
	}
	abiertas := map[int64]bool{}
	for _, ep := range ds.Pendientes {
		abiertas[ep.Ep.MujerID] = true
	}
	for _, c := range ds.Candidatas {
		if !c.Proxima.IsZero() && c.Proxima.Before(fechaActual) && !abiertas[c.ID] {
			cal.Atrasadas++
		}
	}
	porLote := map[string]*mesPlan{}
	for mes := 1; mes <= 12; mes++ {
		lote := fmt.Sprintf("%04d-%02d", anio, mes)
		cal.Meses = append(cal.Meses, mesPlan{Numero: mes, Nombre: nombresMes[mes-1], Lote: lote, Estado: "Previsión"})
	}
	for i := range cal.Meses {
		porLote[cal.Meses[i].Lote] = &cal.Meses[i]
	}
	rows, err := tx.Query(`SELECT periodo,fecha,usuario,salidas FROM planificacion_mensual WHERE periodo>=? AND periodo<=? ORDER BY periodo`, cal.Meses[0].Lote, cal.Meses[11].Lote)
	if err != nil {
		return cal, err
	}
	for rows.Next() {
		var periodo, fecha, usuario string
		var salidas int
		if err := rows.Scan(&periodo, &fecha, &usuario, &salidas); err != nil {
			rows.Close()
			return cal, err
		}
		m := porLote[periodo]
		if m == nil {
			rows.Close()
			return cal, fmt.Errorf("periodo mensual inválido %q", periodo)
		}
		m.Generado, m.Estado, m.Fecha, m.Usuario, m.Salidas = true, "Generado", fecha, usuario, salidas
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return cal, err
	}
	rows, err = tx.Query(`SELECT i.lote,i.tipo,i.prueba,i.motivo,COUNT(*) FROM invitaciones i
	                  JOIN planificacion_mensual p ON p.lote=i.lote
	                  WHERE p.periodo>=? AND p.periodo<=? GROUP BY i.lote,i.tipo,i.prueba,i.motivo`, cal.Meses[0].Lote, cal.Meses[11].Lote)
	if err != nil {
		return cal, err
	}
	for rows.Next() {
		var lote, tipo, prueba, motivo string
		var n int
		if err := rows.Scan(&lote, &tipo, &prueba, &motivo, &n); err != nil {
			rows.Close()
			return cal, err
		}
		if m := porLote[lote]; m != nil {
			m.contar(tipo, prueba, motivo, n)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return cal, err
	}
	// Las citas ya previstas para un mes no se vuelven a contar en los siguientes.
	consumidas := map[int64]bool{}
	proximo, err := time.ParseInLocation("2006-01", cal.Proximo, time.Local)
	if err != nil {
		return cal, err
	}
	if proximo.Year() < anio {
		inicio, _, _ := limitesMes(anio, 1)
		anteriores := calcularPlan(ds, r, proximo, inicio.AddDate(0, 0, -1))
		for _, a := range anteriores.Acciones {
			if a.Invitar || a.Salida {
				consumidas[a.C.ID] = true
			}
		}
	}
	for i := range cal.Meses {
		m := &cal.Meses[i]
		if m.Generado {
			continue
		}
		if m.Lote < cal.Proximo {
			m.Estado = "Fuera de secuencia"
			continue
		}
		desde, hasta, _ := limitesMes(anio, m.Numero)
		p := calcularPlan(ds, r, desde, hasta)
		for _, a := range p.Acciones {
			if consumidas[a.C.ID] {
				continue
			}
			if a.Salida {
				m.Salidas++
				consumidas[a.C.ID] = true
			}
			if a.Invitar {
				m.contar(a.Tipo, a.Prueba, a.Motivo, 1)
				consumidas[a.C.ID] = true
			}
		}
		if m.Lote == cal.Proximo {
			m.PuedeGenerar, m.Estado = true, "Pendiente"
		}
	}
	return cal, nil
}
