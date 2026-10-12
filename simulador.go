package main

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"time"
)

// SIMULADOR: genera actividad SINTÉTICA para probar el programa y los KPIs.
// Usa exactamente las mismas funciones que la importación real
// (registrarPrueba, registrarColposcopia, marcarNoValida, crearExclusion), así
// que también prueba la máquina de estados. Las probabilidades son
// ilustrativas, no datos de La Rioja.

type Simulador struct {
	pg Programa
	r  *rand.Rand
}

type distrib []struct {
	v string
	p float64
}

func (s Simulador) elegir(d distrib) string {
	var tot float64
	for _, x := range d {
		tot += x.p
	}
	u := s.r.Float64() * tot
	for _, x := range d {
		if u < x.p {
			return x.v
		}
		u -= x.p
	}
	return d[len(d)-1].v
}

func (s Simulador) prob(p float64) bool { return s.r.Float64() < p }
func (s Simulador) dias(a, b int) int   { return a + s.r.Intn(b-a+1) }

var (
	dCitoCribado = distrib{{"NEGATIVA", .925}, {"ASCUS", .030}, {"LSIL", .025}, {"ASCH", .004}, {"HSIL", .006},
		{"AGC", .001}, {"AIS", .0003}, {"CARCINOMA", .0002}, {"INSATISFACTORIA", .0085}}
	dCitoControl = distrib{{"NEGATIVA", .85}, {"ASCUS", .07}, {"LSIL", .05}, {"HSIL", .02}, {"INSATISFACTORIA", .01}}
	dCitoTriaje  = distrib{{"NEGATIVA", .68}, {"ASCUS", .15}, {"LSIL", .09}, {"ASCH", .025}, {"HSIL", .03},
		{"AGC", .003}, {"AIS", .0015}, {"CARCINOMA", .0015}, {"INSATISFACTORIA", .02}}
	dVPHProf    = distrib{{"NEGATIVO", .905}, {"POS_1618", .022}, {"POS_OTROS", .068}, {"NO_VALIDO", .005}}
	dVPHAuto    = distrib{{"NEGATIVO", .89}, {"POS_1618", .022}, {"POS_OTROS", .068}, {"NO_VALIDO", .02}}
	dVPHControl = distrib{{"NEGATIVO", .62}, {"POS_1618", .06}, {"POS_OTROS", .31}, {"NO_VALIDO", .01}}
	dCIN2mas    = distrib{{"HSIL_CIN2", .40}, {"HSIL_CIN3", .45}, {"AIS", .05}, {"CARCINOMA_ESCAMOSO", .07},
		{"ADENOCARCINOMA", .025}, {"OTRO_INVASIVO", .005}}
	dMenosCIN2 = distrib{{"NO_LESION", .45}, {"LSIL_CIN1", .53}, {"INSATISFACTORIA", .02}}
	pCIN2mas   = map[string]float64{
		"CITOLOGIA_ALTO_GRADO": .55, "VPH_16_18": .30, "VPH_OTROS_CITOLOGIA_ANORMAL": .22,
		"BAJO_GRADO_VPH_POSITIVO": .15, "VPH_PERSISTENTE_CONTROL": .18, "CONTROL_CITOLOGIA_ANORMAL": .20,
	}
)

type invSim struct {
	id                    int64
	tipo, prueba, muestra string
	motivo                string
	f                     time.Time
}

// simularRespuestas simula qué pasa con cada invitación ENVIADA en el periodo.
func (s Simulador) simularRespuestas(desde, hasta time.Time) (map[string]int, error) {
	cnt := map[string]int{}
	tx, err := s.pg.Begin()
	if err != nil {
		return cnt, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id, tipo, prueba, muestra, motivo, fecha_invitacion FROM invitaciones
	                       WHERE estado='ENVIADA' AND fecha_invitacion BETWEEN ? AND ? ORDER BY id`, fs(desde), fs(hasta))
	if err != nil {
		return cnt, err
	}
	var invs []invSim
	for rows.Next() {
		var x invSim
		var f string
		if err := rows.Scan(&x.id, &x.tipo, &x.prueba, &x.muestra, &x.motivo, &f); err != nil {
			rows.Close()
			return cnt, err
		}
		x.f = mustFecha(f)
		invs = append(invs, x)
	}
	rows.Close()

	for _, x := range invs {
		if x.tipo == "CRIBADO" && s.prob(.03) {
			causa := s.elegir(distrib{{"DIRECCION_INCORRECTA", .5}, {"CARTA_DEVUELTA", .4}, {"DESCONOCIDA", .1}})
			if err := s.pg.marcarNoValida(tx, x.id, causa, x.f.AddDate(0, 0, s.dias(10, 40)), "SIMULADOR"); err != nil {
				return cnt, err
			}
			cnt["no_valida"]++
			continue
		}
		pPart := map[string]float64{"CITOLOGIA PROF": .55, "VPH PROF": .60, "VPH AUTO": .45}[x.prueba+" "+x.muestra]
		if x.tipo == "CONTROL" {
			pPart = .75
		} else if x.motivo == "NO_RESPONDE" && x.muestra == "PROF" {
			pPart *= .5
		}
		if !s.prob(pPart) {
			cnt["no_participa"]++
			continue
		}
		cnt["participa"]++
		if err := s.episodio(tx, x); err != nil {
			return cnt, fmt.Errorf("invitación %d: %w", x.id, err)
		}
	}
	return cnt, tx.Commit()
}

func (s Simulador) episodio(tx *Tx, x invSim) error {
	ft := x.f.AddDate(0, 0, s.dias(3, 90))
	if x.muestra == "AUTO" {
		ft = x.f.AddDate(0, 0, s.dias(7, 45))
	}
	finalidad := "PRIMARIA"
	if x.tipo == "CONTROL" {
		finalidad = "CONTROL"
	}
	var d distrib
	switch {
	case x.prueba == "CITOLOGIA" && x.tipo == "CONTROL":
		d = dCitoControl
	case x.prueba == "CITOLOGIA":
		d = dCitoCribado
	case x.tipo == "CONTROL":
		d = dVPHControl
	case x.muestra == "AUTO":
		d = dVPHAuto
	default:
		d = dVPHProf
	}
	res := s.elegir(d)
	fRes := ft.AddDate(0, 0, s.dias(7, 25)) // fecha del último resultado conocido
	dec, err := s.pg.registrarPrueba(tx, x.id, x.prueba, finalidad, x.muestra, ft, fRes, res, "SIMULADOR")
	if err != nil {
		return err
	}
	ultimoRes := res
	for paso := 0; paso < 8; paso++ {
		switch dec.Accion {
		case accRepetir:
			if !s.prob(.85) {
				return nil
			}
			ft = fRes.AddDate(0, 0, s.dias(15, 45))
			fRes = ft.AddDate(0, 0, s.dias(7, 25))
			res = s.elegir(d)
			ultimoRes = res
			dec, err = s.pg.registrarPrueba(tx, x.id, x.prueba, "REPETICION", x.muestra, ft, fRes, res, "SIMULADOR")
		case accTriajeVPH: // réflex sobre la misma muestra citológica
			pPos := .45
			if ultimoRes == "LSIL" {
				pPos = .75
			}
			r := "NEGATIVO"
			switch {
			case s.prob(.01):
				r = "NO_VALIDO"
			case s.prob(pPos):
				r = s.elegir(distrib{{"POS_1618", .3}, {"POS_OTROS", .7}})
			}
			fRes = fRes.AddDate(0, 0, s.dias(5, 15))
			dec, err = s.pg.registrarPrueba(tx, x.id, "VPH", "TRIAJE", "PROF", ft, fRes, r, "SIMULADOR")
		case accTriajeCito:
			fTri := ft               // réflex sobre la misma muestra
			if x.muestra == "AUTO" { // hay que citarla para una toma por profesional
				if !s.prob(.85) {
					return nil
				}
				fTri = fRes.AddDate(0, 0, s.dias(15, 45))
				fRes = fTri
			}
			fRes = fRes.AddDate(0, 0, s.dias(5, 15))
			dec, err = s.pg.registrarPrueba(tx, x.id, "CITOLOGIA", "TRIAJE", "PROF", fTri, fRes, s.elegir(dCitoTriaje), "SIMULADOR")
		case accColposcopia:
			return s.colposcopia(tx, x.id, dec)
		default:
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (s Simulador) colposcopia(tx *Tx, invID int64, dec Decision) error {
	var fd string
	if err := tx.QueryRow(`SELECT fecha_derivacion FROM colposcopias WHERE invitacion_id=? ORDER BY id DESC LIMIT 1`, invID).Scan(&fd); err != nil {
		return err
	}
	der := mustFecha(fd)
	if !s.prob(.92) {
		return s.pg.registrarColposcopia(tx, invID, "NO_ACUDE", der.AddDate(0, 0, 90), false, false, "", time.Time{}, "SIMULADOR")
	}
	fc := der.AddDate(0, 0, s.dias(15, 75))
	adecuada := s.prob(.94)
	p := pCIN2mas[dec.Motivo]
	if p == 0 {
		p = .10
	}
	biopsia := s.prob(.85)
	if !biopsia {
		return s.pg.registrarColposcopia(tx, invID, "REALIZADA", fc, adecuada, false, "SIN_BIOPSIA", time.Time{}, "SIMULADOR")
	}
	h := s.elegir(dMenosCIN2)
	if s.prob(p) {
		h = s.elegir(dCIN2mas)
	}
	return s.pg.registrarColposcopia(tx, invID, "REALIZADA", fc, adecuada, true, h, fc.AddDate(0, 0, s.dias(10, 25)), "SIMULADOR")
}

// simularExclusionesYBajas genera exclusiones y bajas sintéticas en el año.
func (s Simulador) simularExclusionesYBajas(anio int) (map[string]int, error) {
	cnt := map[string]int{}
	tx, err := s.pg.Begin()
	if err != nil {
		return cnt, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id FROM mujeres WHERE estado='ACTIVA' ORDER BY id`)
	if err != nil {
		return cnt, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return cnt, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	ini := time.Date(anio, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, id := range ids {
		u := s.r.Float64()
		f := ini.AddDate(0, 0, s.r.Intn(365))
		switch {
		case u < .0015:
			err = s.pg.crearExclusion(tx, id, "DEFINITIVA", "HISTERECTOMIA", f, time.Time{}, "SIMULADOR")
			cnt["excl_definitiva"]++
		case u < .0025:
			err = s.pg.crearExclusion(tx, id, "DEFINITIVA", "RECHAZO", f, time.Time{}, "SIMULADOR")
			cnt["excl_definitiva"]++
		case u < .0105:
			err = s.pg.crearExclusion(tx, id, "TEMPORAL", "EMBARAZO", f, f.AddDate(0, s.dias(9, 12), 0), "SIMULADOR")
			cnt["excl_temporal"]++
		case u < .0135:
			err = s.pg.crearExclusion(tx, id, "TEMPORAL", "PRUEBA_RECIENTE", f, f.AddDate(0, s.dias(12, 36), 0), "SIMULADOR")
			cnt["excl_temporal"]++
		case u < .0175:
			if _, err = tx.Exec(`UPDATE mujeres SET estado='BAJA', fecha_estado=?, fecha_proxima=NULL WHERE id=?`, fs(f), id); err == nil {
				err = s.pg.ev(tx, f, id, 0, "BAJA", s.elegir(distrib{{"TRASLADO", .7}, {"FALLECIMIENTO", .3}}), "SIMULADOR")
			}
			cnt["bajas"]++
		}
		if err != nil {
			return cnt, err
		}
	}
	return cnt, tx.Commit()
}

// simularAnio: ciclo completo de un año del programa con datos sintéticos.
func simularAnio(b *BD, anio int, semilla int64, tamCohorte int, dirTrabajo string) error {
	pg := Programa{b, "simulador", "SIMULADOR"}
	s := Simulador{pg: pg, r: rand.New(rand.NewSource(semilla + int64(anio)*7919))}
	ini := time.Date(anio, 1, 1, 0, 0, 0, 0, time.UTC)
	fin := time.Date(anio, 12, 31, 0, 0, 0, 0, time.UTC)
	t0 := time.Now()

	if tamCohorte > 0 {
		ruta := filepath.Join(dirTrabajo, fmt.Sprintf("cohorte_%d.csv", anio))
		if err := generarCohorte(ruta, tamCohorte, anio, semilla); err != nil {
			return err
		}
		rc, err := pg.cargarPadron(ruta, ini, "SIMULADOR")
		if err != nil {
			return err
		}
		os.Remove(ruta)
		fmt.Printf("  [%d] cohorte de 25 años cargada: %d nuevas\n", anio, rc.Nuevas)
	}
	ra, err := pg.altaAnual(anio, "SIMULADOR")
	if err != nil {
		return err
	}
	fmt.Printf("  [%d] alta anual: entran %d, salen por edad %d\n", anio, ra.Entran, ra.Salen)
	ex, err := s.simularExclusionesYBajas(anio)
	if err != nil {
		return err
	}
	fmt.Printf("  [%d] exclusiones/bajas sintéticas: %v\n", anio, ex)
	// Planificación trimestral para que los controles y reinvitaciones del
	// propio año también salgan.
	for q := 0; q < 4; q++ {
		d := ini.AddDate(0, 3*q, 0)
		h := d.AddDate(0, 3, -1)
		rp, err := pg.planificar(d, h, fmt.Sprintf("SIM-%d-T%d", anio, q+1), "SIMULADOR")
		if err != nil {
			return err
		}
		rr, err := s.simularRespuestas(d, h)
		if err != nil {
			return err
		}
		tot := 0
		for _, v := range rp.Invitaciones {
			tot += v
		}
		fmt.Printf("  [%d T%d] invitaciones %d %v · cerradas sin respuesta %d · respuestas %v\n",
			anio, q+1, tot, rp.Invitaciones, rp.CerradasSinRespuesta, rr)
	}
	_ = fin
	fmt.Printf("  [%d] simulado en %s\n", anio, time.Since(t0).Round(time.Millisecond))
	return nil
}
