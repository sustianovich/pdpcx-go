package main

import (
	"fmt"
	"sort"
	"time"
)

// ---------------------------------------------------------------------------
// Catálogos (códigos cerrados: nada de clasificar por texto)
// ---------------------------------------------------------------------------

var resultadosCito = map[string]bool{
	"NEGATIVA": true, "ASCUS": true, "LSIL": true, "ASCH": true, "HSIL": true,
	"AGC": true, "AIS": true, "CARCINOMA": true, "INSATISFACTORIA": true,
}
var resultadosVPH = map[string]bool{
	"NEGATIVO": true, "POS_1618": true, "POS_OTROS": true, "NO_VALIDO": true,
}
var resultadosHisto = map[string]bool{
	"SIN_BIOPSIA": true, "NO_LESION": true, "LSIL_CIN1": true, "HSIL_CIN2": true,
	"HSIL_CIN3": true, "AIS": true, "CARCINOMA_ESCAMOSO": true, "ADENOCARCINOMA": true,
	"OTRO_INVASIVO": true, "INSATISFACTORIA": true,
}

func citoBajoGrado(r string) bool { return r == "ASCUS" || r == "LSIL" }
func citoAltoGrado(r string) bool {
	return r == "ASCH" || r == "HSIL" || r == "AGC" || r == "AIS" || r == "CARCINOMA"
}
func citoSevera(r string) bool  { return r == "AIS" || r == "CARCINOMA" }
func citoAnormal(r string) bool { return citoBajoGrado(r) || citoAltoGrado(r) }
func vphPositivo(r string) bool { return r == "POS_1618" || r == "POS_OTROS" }
func pruebaValida(tipo, r string) bool {
	if tipo == "CITOLOGIA" {
		return r != "INSATISFACTORIA"
	}
	return r != "NO_VALIDO"
}

// Grado histológico: 4 invasivo > 3 AIS > 2 HSIL/CIN2+ > 1 LSIL/CIN1 > 0 otro
func gradoHisto(h string) int {
	switch h {
	case "CARCINOMA_ESCAMOSO", "ADENOCARCINOMA", "OTRO_INVASIVO":
		return 4
	case "AIS":
		return 3
	case "HSIL_CIN2", "HSIL_CIN3":
		return 2
	case "LSIL_CIN1":
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// Decisión clínica (función pura, probada en protocolo_test.go)
// ---------------------------------------------------------------------------

type Prueba struct {
	ID             int64
	Tipo           string
	Finalidad      string
	Muestra        string
	FechaToma      time.Time
	FechaResultado time.Time
	Resultado      string
	Valida         bool
}

type Episodio struct {
	ID      int64
	MujerID int64
	Tipo    string // CRIBADO / CONTROL
	Prueba  string // CITOLOGIA / VPH (prueba primaria del programa)
	Muestra string
	Estado  string
	Pruebas []Prueba
}

const (
	accEsperar      = "ESPERAR"
	accRepetir      = "REPETIR_MUESTRA"
	accTriajeVPH    = "TRIAJE_VPH"
	accTriajeCito   = "TRIAJE_CITOLOGIA"
	accColposcopia  = "DERIVAR_COLPOSCOPIA"
	accControl      = "CONTROL_12M"
	accRutina       = "RUTINA"
	accSinResultado = "SIN_RESULTADO_VALIDO"
)

type Decision struct {
	Accion string
	Motivo string
	Base   time.Time // fecha desde la que se cuentan los intervalos
}

func ultima(ps []Prueba, f func(Prueba) bool) (Prueba, int, bool) {
	var u Prueba
	n, ok := 0, false
	for _, p := range ps {
		if f(p) {
			u, ok = p, true
			n++
		}
	}
	return u, n, ok
}

// decidir aplica el protocolo a las pruebas registradas en un episodio.
//
// Citología (25-29):  NEGATIVA -> rutina | ASCUS/LSIL -> VPH de triaje
//
//	(+ -> colposcopia, - -> control 12 m) | ASC-H, HSIL, AGC, AIS, carcinoma
//	-> colposcopia.
//
// VPH-AR (30-65):     NEGATIVO -> rutina | 16/18 -> colposcopia | otros AR ->
//
//	citología de triaje (anormal -> colposcopia, normal -> control 12 m).
//
// Control 12 m:       VPH + o citología >= ASCUS -> colposcopia; negativo -> rutina.
// Muestra no válida:  se repite hasta max_repeticiones; después se cierra.
func decidir(ep Episodio, maxRep int) Decision {
	ps := append([]Prueba(nil), ep.Pruebas...)
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].FechaToma.Before(ps[j].FechaToma) })

	esPrimaria := func(p Prueba) bool {
		return p.Tipo == ep.Prueba && (p.Finalidad == "PRIMARIA" || p.Finalidad == "REPETICION" || p.Finalidad == "CONTROL")
	}
	prim, _, ok := ultima(ps, esPrimaria)
	if !ok {
		return Decision{Accion: accEsperar}
	}
	_, nInval, _ := ultima(ps, func(p Prueba) bool { return esPrimaria(p) && !p.Valida })
	if !prim.Valida {
		if nInval > maxRep {
			return Decision{accSinResultado, "MUESTRA_PRIMARIA_NO_VALIDA", prim.FechaToma}
		}
		return Decision{accRepetir, "MUESTRA_PRIMARIA_NO_VALIDA", prim.FechaToma}
	}
	base := prim.FechaResultado
	if base.IsZero() {
		base = prim.FechaToma
	}
	tipoTri := "VPH"
	if ep.Prueba == "VPH" {
		tipoTri = "CITOLOGIA"
	}
	esTri := func(p Prueba) bool {
		return p.Finalidad == "TRIAJE" && p.Tipo == tipoTri && !p.FechaToma.Before(prim.FechaToma)
	}
	tri, _, hayTri := ultima(ps, esTri)
	_, nTriInval, _ := ultima(ps, func(p Prueba) bool { return esTri(p) && !p.Valida })
	fTri := tri.FechaResultado
	if fTri.IsZero() {
		fTri = tri.FechaToma
	}

	r := prim.Resultado
	if ep.Prueba == "CITOLOGIA" {
		switch {
		case citoAltoGrado(r):
			return Decision{accColposcopia, "CITOLOGIA_ALTO_GRADO", base}
		case citoBajoGrado(r):
			if ep.Tipo == "CONTROL" {
				return Decision{accColposcopia, "CONTROL_CITOLOGIA_ANORMAL", base}
			}
			if !hayTri || !tri.Valida {
				if hayTri && nTriInval > maxRep {
					return Decision{accColposcopia, "TRIAJE_VPH_NO_VALIDO", fTri}
				}
				return Decision{accTriajeVPH, "CITOLOGIA_BAJO_GRADO", base}
			}
			if vphPositivo(tri.Resultado) {
				return Decision{accColposcopia, "BAJO_GRADO_VPH_POSITIVO", fTri}
			}
			return Decision{accControl, "BAJO_GRADO_VPH_NEGATIVO", fTri}
		default: // NEGATIVA
			return Decision{accRutina, "CITOLOGIA_NEGATIVA", prim.FechaToma}
		}
	}

	// Programa VPH
	switch r {
	case "NEGATIVO":
		return Decision{accRutina, "VPH_NEGATIVO", prim.FechaToma}
	case "POS_1618":
		return Decision{accColposcopia, "VPH_16_18", base}
	}
	// POS_OTROS
	if ep.Tipo == "CONTROL" {
		return Decision{accColposcopia, "VPH_PERSISTENTE_CONTROL", base}
	}
	if !hayTri || !tri.Valida {
		if hayTri && nTriInval > maxRep {
			return Decision{accColposcopia, "TRIAJE_CITOLOGIA_NO_VALIDO", fTri}
		}
		return Decision{accTriajeCito, "VPH_OTROS_AR", base}
	}
	if citoAnormal(tri.Resultado) {
		return Decision{accColposcopia, "VPH_OTROS_CITOLOGIA_ANORMAL", fTri}
	}
	return Decision{accControl, "VPH_OTROS_CITOLOGIA_NEGATIVA", fTri}
}

// ---------------------------------------------------------------------------
// Operaciones sobre la BD (siempre dentro de una transacción)
// ---------------------------------------------------------------------------

func (pg Programa) cargarEpisodio(tx *Tx, invID int64) (Episodio, error) {
	var ep Episodio
	err := tx.QueryRow(`SELECT id, mujer_id, tipo, prueba, muestra, estado FROM invitaciones WHERE id=?`, invID).
		Scan(&ep.ID, &ep.MujerID, &ep.Tipo, &ep.Prueba, &ep.Muestra, &ep.Estado)
	if err != nil {
		return ep, fmt.Errorf("invitación %d: %w", invID, err)
	}
	rows, err := tx.Query(`SELECT id, tipo, finalidad, muestra, fecha_toma, COALESCE(fecha_resultado,''), resultado, valida
	                       FROM pruebas WHERE invitacion_id=? ORDER BY fecha_toma, id`, invID)
	if err != nil {
		return ep, err
	}
	defer rows.Close()
	for rows.Next() {
		var p Prueba
		var ft, fr string
		var v int
		if err := rows.Scan(&p.ID, &p.Tipo, &p.Finalidad, &p.Muestra, &ft, &fr, &p.Resultado, &v); err != nil {
			return ep, err
		}
		p.FechaToma = mustFecha(ft)
		if fr != "" {
			p.FechaResultado = mustFecha(fr)
		}
		p.Valida = v == 1
		ep.Pruebas = append(ep.Pruebas, p)
	}
	return ep, rows.Err()
}

// programar fija la próxima cita de la mujer si sigue activa en el programa.
func (pg Programa) programar(tx *Tx, mujerID int64, f time.Time, motivo string, epControl int64, origen string) error {
	var ctrl any
	if epControl > 0 {
		ctrl = epControl
	}
	res, err := tx.Exec(`UPDATE mujeres SET fecha_proxima=?, motivo_proxima=?, episodio_control=? WHERE id=? AND estado='ACTIVA'`,
		fs(f), motivo, ctrl, mujerID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return pg.ev(tx, f, mujerID, 0, "PROXIMA_CITA", motivo, origen)
	}
	return nil
}

func (pg Programa) cerrar(tx *Tx, ep Episodio, f time.Time, motivo string, proxima time.Time, motivoProx string, origen string) error {
	_, err := tx.Exec(`UPDATE invitaciones SET estado='CERRADA', fecha_cierre=?, motivo_cierre=?, fecha_proxima=? WHERE id=?`,
		fs(f), motivo, nullFecha(proxima), ep.ID)
	if err != nil {
		return err
	}
	if err := pg.ev(tx, f, ep.MujerID, ep.ID, "CIERRE", motivo, origen); err != nil {
		return err
	}
	if proxima.IsZero() {
		return nil
	}
	var ctrl int64
	if motivoProx == "CONTROL" {
		ctrl = ep.ID
	}
	return pg.programar(tx, ep.MujerID, proxima, motivoProx, ctrl, origen)
}

func (pg Programa) cambiarEstado(tx *Tx, ep Episodio, estado string, f time.Time, detalle, origen string) error {
	if ep.Estado == estado {
		return nil
	}
	if _, err := tx.Exec(`UPDATE invitaciones SET estado=? WHERE id=?`, estado, ep.ID); err != nil {
		return err
	}
	return pg.ev(tx, f, ep.MujerID, ep.ID, "ESTADO_"+estado, detalle, origen)
}

// procesar evalúa el episodio tras un nuevo resultado y ejecuta la decisión.
func (pg Programa) procesar(tx *Tx, invID int64, origen string) (Decision, error) {
	ep, err := pg.cargarEpisodio(tx, invID)
	if err != nil {
		return Decision{}, err
	}
	if ep.Estado == "CERRADA" || ep.Estado == "DERIVADA_COLPOSCOPIA" {
		return Decision{Accion: accEsperar}, nil
	}
	d := decidir(ep, pg.pInt("max_repeticiones"))
	if d.Accion != accEsperar {
		if err := pg.registrarDecision(tx, ep, d.Base, d.Accion, d.Motivo); err != nil {
			return d, err
		}
	}
	switch d.Accion {
	case accEsperar:
	case accRepetir:
		err = pg.cambiarEstado(tx, ep, "REPETIR_MUESTRA", d.Base, d.Motivo, origen)
	case accTriajeVPH, accTriajeCito:
		det := d.Accion
		if d.Accion == accTriajeCito && ep.Muestra == "AUTO" {
			det += " (autotoma: citar para toma por profesional)"
		}
		err = pg.cambiarEstado(tx, ep, "TRIAJE_PENDIENTE", d.Base, det, origen)
	case accColposcopia:
		var n int
		if err = tx.QueryRow(`SELECT COUNT(*) FROM colposcopias WHERE invitacion_id=?`, ep.ID).Scan(&n); err != nil {
			break
		}
		if n == 0 {
			if _, err = tx.Exec(`INSERT INTO colposcopias(invitacion_id,mujer_id,fecha_derivacion,motivo_derivacion,estado,origen)
			                     VALUES(?,?,?,?, 'DERIVADA', ?)`, ep.ID, ep.MujerID, fs(d.Base), d.Motivo, origen); err != nil {
				break
			}
		}
		err = pg.cambiarEstado(tx, ep, "DERIVADA_COLPOSCOPIA", d.Base, d.Motivo, origen)
	case accControl:
		err = pg.cerrar(tx, ep, d.Base, "CONTROL_12M", addMeses(d.Base, pg.pInt("control_meses")), "CONTROL", origen)
	case accRutina:
		meses := pg.pInt("intervalo_cito_meses")
		if ep.Prueba == "VPH" {
			meses = pg.pInt("intervalo_vph_meses")
		}
		err = pg.cerrar(tx, ep, d.Base, "RUTINA", addMeses(d.Base, meses), "RUTINA", origen)
	case accSinResultado:
		err = pg.cerrar(tx, ep, d.Base, "SIN_RESULTADO_VALIDO", addMeses(d.Base, pg.pInt("meses_tras_no_responde")), "SIN_RESULTADO", origen)
	}
	return d, err
}

// registrarPrueba guarda un resultado de laboratorio y avanza el episodio.
func (pg Programa) registrarPrueba(tx *Tx, invID int64, tipo, finalidad, muestra string,
	fToma, fRes time.Time, resultado, origen string) (Decision, error) {

	if tipo == "CITOLOGIA" && !resultadosCito[resultado] || tipo == "VPH" && !resultadosVPH[resultado] {
		return Decision{}, fmt.Errorf("resultado %q no válido para %s", resultado, tipo)
	}
	var mujerID int64
	var estado, motivoCierre string
	var fInv string
	err := tx.QueryRow(`SELECT mujer_id, estado, COALESCE(motivo_cierre,''), fecha_invitacion FROM invitaciones WHERE id=?`, invID).
		Scan(&mujerID, &estado, &motivoCierre, &fInv)
	if err != nil {
		return Decision{}, fmt.Errorf("invitación %d: %w", invID, err)
	}
	if fToma.Before(mustFecha(fInv)) {
		return Decision{}, fmt.Errorf("invitación %d: fecha de toma %s anterior a la invitación %s", invID, fs(fToma), fInv)
	}
	// Una muestra que llega tarde reabre un episodio cerrado por falta de respuesta.
	if estado == "CERRADA" {
		if motivoCierre != "NO_RESPONDE" && motivoCierre != "NO_VALIDA" {
			return Decision{}, fmt.Errorf("invitación %d cerrada (%s): no admite más pruebas", invID, motivoCierre)
		}
		if _, err := tx.Exec(`UPDATE invitaciones SET estado='EN_CURSO', fecha_cierre=NULL, motivo_cierre=NULL WHERE id=?`, invID); err != nil {
			return Decision{}, err
		}
		if err := pg.ev(tx, fToma, mujerID, invID, "REAPERTURA", "muestra recibida tras cierre "+motivoCierre, origen); err != nil {
			return Decision{}, err
		}
	} else if estado == "ENVIADA" {
		if _, err := tx.Exec(`UPDATE invitaciones SET estado='EN_CURSO' WHERE id=?`, invID); err != nil {
			return Decision{}, err
		}
	}
	valida := 0
	if pruebaValida(tipo, resultado) {
		valida = 1
	}
	_, err = tx.Exec(`INSERT INTO pruebas(invitacion_id,mujer_id,tipo,finalidad,muestra,fecha_toma,fecha_resultado,resultado,valida,origen)
	                  VALUES(?,?,?,?,?,?,?,?,?,?)`, invID, mujerID, tipo, finalidad, muestra, fs(fToma), nullFecha(fRes), resultado, valida, origen)
	if err != nil {
		return Decision{}, err
	}
	if err := pg.ev(tx, fToma, mujerID, invID, "PRUEBA_"+tipo, finalidad+" "+muestra+" -> "+resultado, origen); err != nil {
		return Decision{}, err
	}
	return pg.procesar(tx, invID, origen)
}

// registrarColposcopia: estado REALIZADA o NO_ACUDE. Con histología CIN2+ la
// mujer pasa a SEGUIMIENTO_CLINICO (sale del cribado hasta su reincorporación).
func (pg Programa) registrarColposcopia(tx *Tx, invID int64, estado string, f time.Time,
	adecuada, biopsia bool, histologia string, fHist time.Time, origen string) error {

	if estado != "REALIZADA" && estado != "NO_ACUDE" {
		return fmt.Errorf("estado de colposcopia no válido: %s", estado)
	}
	if histologia != "" && !resultadosHisto[histologia] {
		return fmt.Errorf("histología no válida: %s", histologia)
	}
	if estado == "REALIZADA" && !biopsia && histologia == "" {
		histologia = "SIN_BIOPSIA"
	}
	var colID int64
	if err := tx.QueryRow(`SELECT id FROM colposcopias WHERE invitacion_id=? ORDER BY id DESC LIMIT 1`, invID).Scan(&colID); err != nil {
		return fmt.Errorf("invitación %d sin derivación a colposcopia: %w", invID, err)
	}
	b2i := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	var hist any
	if histologia != "" {
		hist = histologia
	}
	_, err := tx.Exec(`UPDATE colposcopias SET estado=?, fecha_realizacion=?, adecuada=?, biopsia=?, histologia=?, fecha_histologia=? WHERE id=?`,
		estado, fs(f), b2i(adecuada), b2i(biopsia), hist, nullFecha(fHist), colID)
	if err != nil {
		return err
	}
	ep, err := pg.cargarEpisodio(tx, invID)
	if err != nil {
		return err
	}
	if err := pg.ev(tx, f, ep.MujerID, invID, "COLPOSCOPIA_"+estado, histologia, origen); err != nil {
		return err
	}
	if histologia != "" || estado == "NO_ACUDE" {
		acc := "ALTA_COLPOSCOPIA"
		switch {
		case estado == "NO_ACUDE":
			acc = "NO_ACUDE_COLPOSCOPIA"
		case gradoHisto(histologia) >= 2:
			acc = "SALIDA_SEGUIMIENTO_CLINICO"
		}
		if err := pg.registrarDecision(tx, ep, f, acc, histologia); err != nil {
			return err
		}
	}
	switch {
	case estado == "NO_ACUDE":
		return pg.cerrar(tx, ep, f, "NO_ACUDE_COLPOSCOPIA", addMeses(f, pg.pInt("meses_tras_no_responde")), "NO_ACUDE_COLPOSCOPIA", origen)
	case histologia == "":
		return nil // pendiente de histología
	case gradoHisto(histologia) >= 2:
		fc := fHist
		if fc.IsZero() {
			fc = f
		}
		if err := pg.cerrar(tx, ep, fc, "DERIVADA_TRATAMIENTO", time.Time{}, "", origen); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE mujeres SET estado='SEGUIMIENTO_CLINICO', fecha_estado=?, fecha_proxima=NULL, motivo_proxima=NULL WHERE id=?`, fs(fc), ep.MujerID); err != nil {
			return err
		}
		return pg.ev(tx, fc, ep.MujerID, invID, "SALIDA_SEGUIMIENTO_CLINICO", histologia, origen)
	default:
		fc := fHist
		if fc.IsZero() {
			fc = f
		}
		return pg.cerrar(tx, ep, fc, "ALTA_COLPOSCOPIA", addMeses(fc, pg.pInt("meses_tras_alta_colposcopia")), "TRAS_COLPOSCOPIA", origen)
	}
}

// marcarNoValida: la invitación no llegó (dirección errónea, devuelta, ...).
func (pg Programa) marcarNoValida(tx *Tx, invID int64, causa string, f time.Time, origen string) error {
	ep, err := pg.cargarEpisodio(tx, invID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE invitaciones SET valida=0, causa_no_valida=? WHERE id=?`, causa, invID); err != nil {
		return err
	}
	if err := pg.ev(tx, f, ep.MujerID, invID, "INVITACION_NO_VALIDA", causa, origen); err != nil {
		return err
	}
	if ep.Estado != "ENVIADA" {
		return nil // ya acudió: se conserva el episodio
	}
	return pg.cerrar(tx, ep, f, "NO_VALIDA", addMeses(f, pg.pInt("meses_tras_no_valida")), "NO_VALIDA", origen)
}

type pendienteRespuesta struct {
	Ep   Episodio
	Base time.Time
}

// También lo usa la previsión: leer los posibles cierres no modifica episodios.
func leerPendientesRespuesta(tx *Tx) ([]pendienteRespuesta, error) {
	rows, err := tx.Query(`
	    SELECT i.id, i.mujer_id, i.tipo, i.prueba, i.muestra, i.estado,
	           COALESCE((SELECT MAX(COALESCE(p.fecha_resultado,p.fecha_toma)) FROM pruebas p WHERE p.invitacion_id=i.id), i.fecha_invitacion)
	    FROM invitaciones i
	    WHERE i.estado<>'CERRADA' ORDER BY i.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ps []pendienteRespuesta
	for rows.Next() {
		var p pendienteRespuesta
		var f string
		if err := rows.Scan(&p.Ep.ID, &p.Ep.MujerID, &p.Ep.Tipo, &p.Ep.Prueba, &p.Ep.Muestra, &p.Ep.Estado, &f); err != nil {
			return nil, err
		}
		p.Base, err = parseFecha(f)
		if err != nil {
			return nil, err
		}
		ps = append(ps, p)
	}
	return ps, rows.Err()
}

func cierresRespuesta(ps []pendienteRespuesta, corte time.Time, plazo int) []pendienteRespuesta {
	var cierres []pendienteRespuesta
	for _, p := range ps {
		switch p.Ep.Estado {
		case "ENVIADA", "REPETIR_MUESTRA", "TRIAJE_PENDIENTE":
			if p.Base.AddDate(0, 0, plazo).Before(corte) {
				cierres = append(cierres, p)
			}
		}
	}
	return cierres
}

func (pg Programa) aplicarCierresRespuesta(tx *Tx, ps []pendienteRespuesta, plazo, meses int, origen string) error {
	for _, p := range ps {
		fc := p.Base.AddDate(0, 0, plazo)
		motivo := map[string]string{"ENVIADA": "NO_RESPONDE", "REPETIR_MUESTRA": "REPETICION_NO_REALIZADA", "TRIAJE_PENDIENTE": "TRIAJE_NO_REALIZADO"}[p.Ep.Estado]
		if err := pg.cerrar(tx, p.Ep, fc, motivo, addMeses(p.Base, meses), "NO_RESPONDE", origen); err != nil {
			return err
		}
	}
	return nil
}

// cerrarSinRespuesta cierra invitaciones sin actividad pasado el plazo.
func (pg Programa) cerrarSinRespuesta(tx *Tx, corte time.Time, origen string) (int, error) {
	ps, err := leerPendientesRespuesta(tx)
	if err != nil {
		return 0, err
	}
	plazo := pg.pInt("plazo_respuesta_dias")
	ps = cierresRespuesta(ps, corte, plazo)
	if err := pg.aplicarCierresRespuesta(tx, ps, plazo, pg.pInt("meses_tras_no_responde"), origen); err != nil {
		return 0, err
	}
	return len(ps), nil
}

// registrarDecision deja constancia de qué decidió el motor de reglas, con qué
// versión de parámetros y con qué versión del protocolo.
func (pg Programa) registrarDecision(tx *Tx, ep Episodio, f time.Time, accion, motivo string) error {
	_, err := tx.Exec(`INSERT INTO decisiones(fecha,invitacion_id,mujer_id,accion,motivo,version_reglas,protocolo,usuario)
	                   VALUES(?,?,?,?,?,?,?,?)`, fs(f), ep.ID, ep.MujerID, accion, motivo, pg.verReglas(), protocoloVersion, pg.Usuario)
	return err
}
