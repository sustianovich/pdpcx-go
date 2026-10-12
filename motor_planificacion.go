package main

import (
	"fmt"
	"sort"
	"strconv"
	"time"
)

type reglasPlan struct {
	CitoMin, CitoMax, VPHMin, VPHMax int
	Plazo, MesesNoRespuesta          int
	Autotoma                         time.Time
	Estrategia                       string
	Version                          int64
}

func (pg Programa) reglasPlanificacion() (reglasPlan, error) {
	pg.mu.RLock()
	defer pg.mu.RUnlock()
	r := reglasPlan{Estrategia: pg.par["autotoma_estrategia"], Version: pg.version}
	for k, destino := range map[string]*int{
		"edad_cito_min": &r.CitoMin, "edad_cito_max": &r.CitoMax,
		"edad_vph_min": &r.VPHMin, "edad_vph_max": &r.VPHMax,
		"plazo_respuesta_dias": &r.Plazo, "meses_tras_no_responde": &r.MesesNoRespuesta,
	} {
		n, err := strconv.Atoi(pg.par[k])
		if err != nil || n < 0 {
			return r, fmt.Errorf("parámetro %s no válido", k)
		}
		*destino = n
	}
	if r.CitoMin > r.CitoMax || r.VPHMin > r.VPHMax || r.CitoMax+1 != r.VPHMin {
		return r, fmt.Errorf("rangos de edad incoherentes: citología y VPH deben ser contiguos y no solaparse; revise edad_cito_max y edad_vph_min")
	}
	if s := pg.par["autotoma_inicio"]; s != "" {
		var err error
		r.Autotoma, err = parseFecha(s)
		if err != nil {
			return r, err
		}
	}
	return r, nil
}

type candidataPlan struct {
	ID                  int64
	Nacimiento, Proxima time.Time
	Motivo              string
	Control             int64
	Ronda               int
}

type tramoPlan struct{ Inicio, Fin time.Time }
type datosPlan struct {
	Candidatas  []candidataPlan
	Pendientes  []pendienteRespuesta
	Exclusiones map[int64][]tramoPlan
}

// Una lectura por conjunto; el calendario reutiliza estos datos en memoria.
func leerDatosPlan(tx *Tx) (datosPlan, error) {
	ds := datosPlan{Exclusiones: map[int64][]tramoPlan{}}
	rows, err := tx.Query(`SELECT m.id, m.fecha_nacimiento, COALESCE(m.fecha_proxima,''),
	                             COALESCE(m.motivo_proxima,''), COALESCE(m.episodio_control,0),
	                             (SELECT COUNT(*) FROM invitaciones i WHERE i.mujer_id=m.id AND i.tipo='CRIBADO')
	                      FROM mujeres m WHERE m.estado='ACTIVA' ORDER BY m.id`)
	if err != nil {
		return ds, err
	}
	for rows.Next() {
		var c candidataPlan
		var fn, fp string
		if err := rows.Scan(&c.ID, &fn, &fp, &c.Motivo, &c.Control, &c.Ronda); err != nil {
			rows.Close()
			return ds, err
		}
		c.Nacimiento, err = parseFecha(fn)
		if err == nil && fp != "" {
			c.Proxima, err = parseFecha(fp)
		}
		if err != nil {
			rows.Close()
			return ds, err
		}
		ds.Candidatas = append(ds.Candidatas, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return ds, err
	}
	ds.Pendientes, err = leerPendientesRespuesta(tx)
	if err != nil {
		return ds, err
	}
	rows, err = tx.Query(`SELECT mujer_id, fecha_inicio, COALESCE(fecha_fin,'9999-12-31')
	                     FROM exclusiones WHERE tipo='TEMPORAL' ORDER BY fecha_inicio, fecha_fin, id`)
	if err != nil {
		return ds, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var a, z string
		if err := rows.Scan(&id, &a, &z); err != nil {
			return ds, err
		}
		ini, err := parseFecha(a)
		if err != nil {
			return ds, err
		}
		fin, err := parseFecha(z)
		if err != nil || fin.Before(ini) {
			return ds, fmt.Errorf("exclusión temporal con fechas inválidas para la mujer %d", id)
		}
		ds.Exclusiones[id] = append(ds.Exclusiones[id], tramoPlan{ini, fin})
	}
	return ds, rows.Err()
}

type accionPlan struct {
	C                             candidataPlan
	Fecha                         time.Time
	Aplazada, Salida, Invitar     bool
	Edad                          int
	Tipo, Prueba, Muestra, Motivo string
}
type calculoPlan struct {
	Cierres    []pendienteRespuesta
	Acciones   []accionPlan
	Candidatas int
}

// Función pura: proyecta los cierres y aplica las mismas reglas para ver y generar.
func calcularPlan(ds datosPlan, r reglasPlan, desde, hasta time.Time) calculoPlan {
	p := calculoPlan{Cierres: cierresRespuesta(ds.Pendientes, desde, r.Plazo)}
	cierran := map[int64]bool{}
	reprogramadas := map[int64]time.Time{}
	for _, c := range p.Cierres {
		cierran[c.Ep.ID] = true
		reprogramadas[c.Ep.MujerID] = addMeses(c.Base, r.MesesNoRespuesta)
	}
	abiertas := map[int64]bool{}
	for _, ep := range ds.Pendientes {
		if !cierran[ep.Ep.ID] {
			abiertas[ep.Ep.MujerID] = true
		}
	}
	cs := append([]candidataPlan(nil), ds.Candidatas...)
	for i := range cs {
		if f, ok := reprogramadas[cs[i].ID]; ok {
			cs[i].Proxima, cs[i].Motivo, cs[i].Control = f, "NO_RESPONDE", 0
		}
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].Proxima.Equal(cs[j].Proxima) {
			return cs[i].ID < cs[j].ID
		}
		return cs[i].Proxima.Before(cs[j].Proxima)
	})
	for _, c := range cs {
		if abiertas[c.ID] || c.Proxima.IsZero() || c.Proxima.After(hasta) {
			continue
		}
		p.Candidatas++
		a := accionPlan{C: c, Fecha: maxT(c.Proxima, desde)}
		for _, t := range ds.Exclusiones[c.ID] {
			if !a.Fecha.Before(t.Inicio) && !a.Fecha.After(t.Fin) {
				a.Fecha = t.Fin.AddDate(0, 0, 1)
				a.Aplazada = true
			}
		}
		if a.Fecha.After(hasta) {
			p.Acciones = append(p.Acciones, a)
			continue
		}
		a.Edad = edad(c.Nacimiento, a.Fecha)
		if a.Edad > r.VPHMax {
			a.Salida = true
			p.Acciones = append(p.Acciones, a)
			continue
		}
		switch {
		case a.Edad >= r.CitoMin && a.Edad <= r.CitoMax:
			a.Prueba = "CITOLOGIA"
		case a.Edad >= r.VPHMin && a.Edad <= r.VPHMax:
			a.Prueba = "VPH"
		default:
			p.Acciones = append(p.Acciones, a)
			continue
		}
		a.Tipo, a.Motivo = "CRIBADO", c.Motivo
		if c.Motivo == "CONTROL" {
			a.Tipo = "CONTROL"
		}
		if a.Motivo == "" {
			a.Motivo = "RUTINA"
		}
		a.Muestra = "PROF"
		if a.Prueba == "VPH" && !r.Autotoma.IsZero() && !a.Fecha.Before(r.Autotoma) &&
			(r.Estrategia == "TODAS" || r.Estrategia == "NO_RESPONDEDORAS" && a.Tipo == "CRIBADO" && c.Motivo == "NO_RESPONDE") {
			a.Muestra = "AUTO"
		}
		a.Invitar = true
		p.Acciones = append(p.Acciones, a)
	}
	return p
}

func bloquearPlanificacion(tx *Tx) error {
	_, err := tx.Exec(`UPDATE planificacion_control SET id=id WHERE id=1`)
	return err
}

func comprobarLoteNuevo(tx *Tx, lote string) error {
	if lote == "" {
		return fmt.Errorf("indique el nombre del lote")
	}
	var n int
	if err := tx.QueryRow(`SELECT (SELECT COUNT(*) FROM cargas WHERE tipo='PLANIFICACION' AND fichero=?) +
	                           (SELECT COUNT(*) FROM invitaciones WHERE lote=?)`, lote, lote).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("ya existe un lote con el nombre %s", lote)
	}
	return nil
}

func (pg Programa) planificarEnTx(tx *Tx, desde, hasta time.Time, lote, origen string) (resumenPlan, error) {
	rs := resumenPlan{Invitaciones: map[string]int{}}
	if desde.IsZero() || hasta.Before(desde) {
		return rs, fmt.Errorf("periodo de planificación no válido")
	}
	r, err := pg.reglasPlanificacion()
	if err != nil {
		return rs, err
	}
	ds, err := leerDatosPlan(tx)
	if err != nil {
		return rs, err
	}
	p := calcularPlan(ds, r, desde, hasta)
	if err := pg.aplicarCierresRespuesta(tx, p.Cierres, r.Plazo, r.MesesNoRespuesta, origen); err != nil {
		return rs, err
	}
	rs.CerradasSinRespuesta = len(p.Cierres)
	ins, err := tx.Prepare(`INSERT INTO invitaciones(mujer_id,tipo,episodio_origen,ronda,fecha_invitacion,edad,prueba,muestra,motivo,valida,estado,lote,version_reglas)
	                    VALUES(?,?,?,?,?,?,?,?,?,1,'ENVIADA',?,?) RETURNING id`)
	if err != nil {
		return rs, err
	}
	defer ins.Close()
	for _, a := range p.Acciones {
		if a.Aplazada {
			rs.AplazadasExclusion++
			if _, err := tx.Exec(`UPDATE mujeres SET fecha_proxima=? WHERE id=?`, fs(a.Fecha), a.C.ID); err != nil {
				return rs, err
			}
			if err := pg.ev(tx, desde, a.C.ID, 0, "APLAZADA_EXCLUSION_TEMPORAL", "nueva fecha "+fs(a.Fecha), origen); err != nil {
				return rs, err
			}
		}
		if a.Salida {
			f := cumple(a.C.Nacimiento, r.VPHMax+1)
			if _, err := tx.Exec(`UPDATE mujeres SET estado='SALIDA_EDAD',fecha_estado=?,fecha_proxima=NULL,motivo_proxima=NULL,episodio_control=NULL WHERE id=?`, fs(f), a.C.ID); err != nil {
				return rs, err
			}
			if err := pg.ev(tx, f, a.C.ID, 0, "SALIDA_EDAD", fmt.Sprintf("próxima cita %s posterior a cumplir %d", fs(a.Fecha), r.VPHMax+1), origen); err != nil {
				return rs, err
			}
			rs.SalenPorEdad++
		}
		if !a.Invitar {
			continue
		}
		var ctrl any
		if a.Tipo == "CONTROL" && a.C.Control > 0 {
			ctrl = a.C.Control
		}
		ronda := a.C.Ronda
		if a.Tipo == "CRIBADO" {
			ronda++
		}
		var id int64
		if err := ins.QueryRow(a.C.ID, a.Tipo, ctrl, ronda, fs(a.Fecha), a.Edad, a.Prueba, a.Muestra, a.Motivo, lote, r.Version).Scan(&id); err != nil {
			return rs, err
		}
		if err := pg.ev(tx, a.Fecha, a.C.ID, id, "INVITACION", fmt.Sprintf("%s %s %s (%s, %d años)", a.Tipo, a.Prueba, a.Muestra, a.Motivo, a.Edad), origen); err != nil {
			return rs, err
		}
		rs.Invitaciones[a.Tipo+" "+a.Prueba+" "+a.Muestra]++
	}
	err = pg.registrarCarga(tx, "PLANIFICACION", "PROCESO", lote, p.Candidatas, fmt.Sprintf("%s..%s %+v", fs(desde), fs(hasta), rs))
	return rs, err
}
