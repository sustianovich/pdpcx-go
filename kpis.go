package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// KPIs con la estructura de la plantilla de la Red (CERVIX_02 Participación y
// CERVIX_03 Resultados). Todo se cuenta en PERSONAS.
//
// Participación: cohorte = mujeres con invitación de CRIBADO fechada en el año
//   (una invitación por mujer: válida > con resultado > la primera).
// Resultados:    cohorte = mujeres con muestra primaria válida tomada en el año
//   (invitación de CRIBADO válida). Una mujer con varios episodios cuenta una
//   vez, en su categoría más grave, para que se cumplan las sumas de la
//   plantilla (C18=C19+C20, C22=C23+C24, C35=C36+C37, C39=C40+C41+C42, ...).
// Los controles a 12 meses (tipo CONTROL) no entran en estas celdas.

type Celda struct {
	Hoja, Celda, Desc string
	Valor             *int
}

type Informe struct {
	Anio        int
	Celdas      []Celda
	Indicadores [][3]string // nombre, valor, fórmula
	Controles   [][2]string // comprobación, resultado
}

func iv(n int) *int { return &n }

type epRes struct {
	mujer          int64
	prueba         string
	muestra        string
	valido         bool
	citoCat        int // 0 neg, 1 bajo, 2 alto
	triVPH         bool
	tri1618        bool
	triOtros       bool
	vphCat         int // 0 neg, 1 otros AR, 2 16/18
	triCito        bool
	triCitoCat     int // 0 neg, 1 bajo, 2 alto, 3 severa
	colpoDerivada  bool
	colpoAdecuada  bool
	histo          int // 0..4
	diasDerivColpo int // -1 si no consta
}

func (b *BD) calcularKPIs(anio int) (*Informe, error) {
	ini := time.Date(anio, 1, 1, 0, 0, 0, 0, time.UTC)
	fin := time.Date(anio, 12, 31, 0, 0, 0, 0, time.UTC)
	inf := &Informe{Anio: anio}
	citoMin, citoMax := b.pInt("edad_cito_min"), b.pInt("edad_cito_max")
	vphMin, vphMax := b.pInt("edad_vph_min"), b.pInt("edad_vph_max")

	// ---------------- Población y exclusiones ----------------
	excl := map[int64]bool{}
	rows, err := b.Query(`SELECT DISTINCT mujer_id FROM exclusiones WHERE fecha_inicio<=? AND COALESCE(fecha_fin,'9999-12-31')>=?`, fs(fin), fs(ini))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		excl[id] = true
	}
	rows.Close()
	var pobC, pobV, excC, excV int
	rows, err = b.Query(`SELECT id, fecha_nacimiento, estado, fecha_estado FROM mujeres WHERE fecha_alta<=? AND estado<>'FUSIONADA'`, fs(fin))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var fn, est, fe string
		if err := rows.Scan(&id, &fn, &est, &fe); err != nil {
			rows.Close()
			return nil, err
		}
		if est == "BAJA" && mustFecha(fe).Before(ini) {
			continue
		}
		e := edad(mustFecha(fn), ini)
		switch {
		case e >= citoMin && e <= citoMax:
			pobC++
			if excl[id] {
				excC++
			}
		case e >= vphMin && e <= vphMax:
			pobV++
			if excl[id] {
				excV++
			}
		}
	}
	rows.Close()

	// ---------------- Participación ----------------
	type invP struct {
		id, mujer       int64
		prueba, muestra string
		valida, res     bool
		f               string
	}
	rows, err = b.Query(`
	    SELECT i.id, i.mujer_id, i.prueba, i.muestra, i.valida, i.fecha_invitacion,
	           CASE WHEN EXISTS (SELECT 1 FROM pruebas p WHERE p.invitacion_id=i.id AND p.tipo=i.prueba
	                   AND p.finalidad IN ('PRIMARIA','REPETICION') AND p.valida=1) THEN 1 ELSE 0 END
	    FROM invitaciones i
	    WHERE i.tipo='CRIBADO' AND i.fecha_invitacion BETWEEN ? AND ?`, fs(ini), fs(fin))
	if err != nil {
		return nil, err
	}
	mejor := map[int64]invP{}
	mejorQue := func(a, c invP) bool { // ¿a es mejor que c?
		if a.valida != c.valida {
			return a.valida
		}
		if a.res != c.res {
			return a.res
		}
		if a.f != c.f {
			return a.f < c.f
		}
		return a.id < c.id
	}
	for rows.Next() {
		var x invP
		var v, r int
		if err := rows.Scan(&x.id, &x.mujer, &x.prueba, &x.muestra, &v, &x.f, &r); err != nil {
			rows.Close()
			return nil, err
		}
		x.valida, x.res = v == 1, r == 1
		if c, ok := mejor[x.mujer]; !ok || mejorQue(x, c) {
			mejor[x.mujer] = x
		}
	}
	rows.Close()
	var p = map[string]int{}
	for _, x := range mejor {
		k := x.prueba + "_" + x.muestra
		if x.prueba == "CITOLOGIA" {
			k = "CITO"
		}
		p[k+"_inv"]++
		if !x.valida {
			p[k+"_novalida"]++
		}
		if x.valida && x.res {
			p[k+"_res"]++
		}
	}

	// ---------------- Resultados ----------------
	eps := map[int64]*epRes{}
	rows, err = b.Query(`
	    SELECT p.invitacion_id, i.mujer_id, i.prueba, p.tipo, p.finalidad, p.muestra, p.fecha_toma, p.resultado, p.valida
	    FROM pruebas p JOIN invitaciones i ON i.id=p.invitacion_id
	    WHERE i.tipo='CRIBADO' AND i.valida=1
	      AND i.id IN (SELECT invitacion_id FROM pruebas
	                   WHERE fecha_toma BETWEEN ? AND ? AND finalidad IN ('PRIMARIA','REPETICION') AND valida=1)
	    ORDER BY p.invitacion_id, p.fecha_toma, p.id`, fs(ini), fs(fin))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var inv, mujer int64
		var prog, tipo, finalidad, muestra, ft, r string
		var v int
		if err := rows.Scan(&inv, &mujer, &prog, &tipo, &finalidad, &muestra, &ft, &r, &v); err != nil {
			rows.Close()
			return nil, err
		}
		e := eps[inv]
		if e == nil {
			e = &epRes{mujer: mujer, prueba: prog, diasDerivColpo: -1}
			eps[inv] = e
		}
		if v != 1 {
			continue
		}
		enAnio := ft >= fs(ini) && ft <= fs(fin)
		switch {
		case (finalidad == "PRIMARIA" || finalidad == "REPETICION") && tipo == prog && enAnio:
			if !e.valido {
				e.muestra = muestra
			}
			e.valido = true
			if prog == "CITOLOGIA" {
				c := 0
				if citoBajoGrado(r) {
					c = 1
				}
				if citoAltoGrado(r) {
					c = 2
				}
				e.citoCat = max(e.citoCat, c)
			} else {
				c := map[string]int{"POS_OTROS": 1, "POS_1618": 2}[r]
				e.vphCat = max(e.vphCat, c)
			}
		case finalidad == "TRIAJE" && tipo == "VPH":
			e.triVPH = true
			e.tri1618 = e.tri1618 || r == "POS_1618"
			e.triOtros = e.triOtros || r == "POS_OTROS"
		case finalidad == "TRIAJE" && tipo == "CITOLOGIA":
			e.triCito = true
			c := 0
			switch {
			case citoSevera(r):
				c = 3
			case citoAltoGrado(r):
				c = 2
			case citoBajoGrado(r):
				c = 1
			}
			e.triCitoCat = max(e.triCitoCat, c)
		}
	}
	rows.Close()
	rows, err = b.Query(`
	    SELECT c.invitacion_id, c.estado, COALESCE(c.adecuada,0), COALESCE(c.histologia,''), c.fecha_derivacion, COALESCE(c.fecha_realizacion,'')
	    FROM colposcopias c JOIN invitaciones i ON i.id=c.invitacion_id
	    WHERE i.tipo='CRIBADO' AND i.valida=1
	      AND i.id IN (SELECT invitacion_id FROM pruebas
	                   WHERE fecha_toma BETWEEN ? AND ? AND finalidad IN ('PRIMARIA','REPETICION') AND valida=1)`, fs(ini), fs(fin))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var inv int64
		var est, h, fd, fr string
		var ad int
		if err := rows.Scan(&inv, &est, &ad, &h, &fd, &fr); err != nil {
			rows.Close()
			return nil, err
		}
		e := eps[inv]
		if e == nil {
			continue
		}
		e.colpoDerivada = true
		if est == "REALIZADA" {
			if ad == 1 {
				e.colpoAdecuada = true
			}
			e.histo = max(e.histo, gradoHisto(h))
			if fr != "" {
				e.diasDerivColpo = int(mustFecha(fr).Sub(mustFecha(fd)).Hours() / 24)
			}
		}
	}
	rows.Close()

	// Agregación por mujer (citología: una clave; VPH: por tipo de muestra)
	type agg struct {
		valid                 bool
		cat, triCat           int
		triVPH, t1618, tOtros bool
		triCito               bool
		deriv, adec           bool
		histo                 int
	}
	porMujer := map[string]*agg{}
	var dias []int
	for _, e := range eps {
		if !e.valido {
			continue
		}
		k := fmt.Sprintf("CITO|%d", e.mujer)
		if e.prueba == "VPH" {
			k = fmt.Sprintf("VPH_%s|%d", e.muestra, e.mujer)
		}
		a := porMujer[k]
		if a == nil {
			a = &agg{}
			porMujer[k] = a
		}
		a.valid = true
		if e.prueba == "CITOLOGIA" {
			a.cat = max(a.cat, e.citoCat)
			if e.citoCat >= 1 {
				a.triVPH = a.triVPH || e.triVPH
				a.t1618 = a.t1618 || e.tri1618
				a.tOtros = a.tOtros || e.triOtros
			}
		} else {
			a.cat = max(a.cat, e.vphCat)
			if e.vphCat == 1 {
				a.triCito = a.triCito || e.triCito
				a.triCat = max(a.triCat, e.triCitoCat)
			}
		}
		a.deriv = a.deriv || e.colpoDerivada
		a.adec = a.adec || e.colpoAdecuada
		a.histo = max(a.histo, e.histo)
		if e.diasDerivColpo >= 0 {
			dias = append(dias, e.diasDerivColpo)
		}
	}
	r := map[string]int{}
	for k, a := range porMujer {
		g := k[:strings.Index(k, "|")]
		r[g+"_val"]++
		if g == "CITO" {
			if a.cat == 1 {
				r[g+"_bajo"]++
			}
			if a.cat == 2 {
				r[g+"_alto"]++
			}
			if a.triVPH {
				r[g+"_trivph"]++
			}
			if a.t1618 {
				r[g+"_t1618"]++
			} else if a.tOtros {
				r[g+"_totros"]++
			}
		} else {
			if a.cat == 2 {
				r[g+"_1618"]++
			}
			if a.cat == 1 {
				r[g+"_otros"]++
				if a.triCito {
					r[g+"_tricito"]++
				}
				switch a.triCat {
				case 1:
					r[g+"_tbajo"]++
				case 2:
					r[g+"_talto"]++
				case 3:
					r[g+"_tsev"]++
				}
			}
		}
		if a.deriv {
			r[g+"_deriv"]++
		}
		if a.adec {
			r[g+"_adec"]++
		}
		if a.histo >= 1 {
			r[fmt.Sprintf("%s_h%d", g, a.histo)]++
		}
	}

	// ---------------- Celdas ----------------
	var ineC, ineV *int
	if v := b.getPar("ine_poblacion_cito"); v != "" {
		n := 0
		fmt.Sscan(v, &n)
		ineC = iv(n)
	}
	if v := b.getPar("ine_poblacion_vph"); v != "" {
		n := 0
		fmt.Sscan(v, &n)
		ineV = iv(n)
	}
	P, R := "Participación", "Resultados"
	add := func(h, c, d string, v *int) { inf.Celdas = append(inf.Celdas, Celda{h, c, d, v}) }
	add(P, "C4", "Población diana INE - citología (parámetro ine_poblacion_cito)", ineC)
	add(P, "D4", "Población diana INE - VPH-AR (parámetro ine_poblacion_vph)", ineV)
	add(P, "C5", fmt.Sprintf("Población diana del programa - citología (%d-%d a 01/01)", citoMin, citoMax), iv(pobC))
	add(P, "D5", fmt.Sprintf("Población diana del programa - VPH-AR (%d-%d a 01/01)", vphMin, vphMax), iv(pobV))
	add(P, "C6", "Mujeres excluidas (vigente algún día del año) - citología", iv(excC))
	add(P, "D6", "Mujeres excluidas (vigente algún día del año) - VPH-AR", iv(excV))
	add(P, "C15", "Invitadas - citología", iv(p["CITO_inv"]))
	add(P, "C16", "Invitación no válida - citología", iv(p["CITO_novalida"]))
	add(P, "C18", "Invitación válida y resultado válido - citología", iv(p["CITO_res"]))
	add(P, "C21", "Invitadas - VPH muestra profesional", iv(p["VPH_PROF_inv"]))
	add(P, "D21", "Invitadas - VPH autotoma", iv(p["VPH_AUTO_inv"]))
	add(P, "C22", "Invitación no válida - VPH profesional", iv(p["VPH_PROF_novalida"]))
	add(P, "D22", "Invitación no válida - VPH autotoma", iv(p["VPH_AUTO_novalida"]))
	add(P, "C24", "Invitación válida y resultado VPH válido - profesional", iv(p["VPH_PROF_res"]))
	add(P, "D24", "Invitación válida y resultado VPH válido - autotoma", iv(p["VPH_AUTO_res"]))

	add(R, "C17", "Resultado válido citología primaria", iv(r["CITO_val"]))
	add(R, "C19", "Bajo grado (ASC-US/LSIL)", iv(r["CITO_bajo"]))
	add(R, "C20", "Alto grado (ASC-H/HSIL/AGC/AIS/carcinoma)", iv(r["CITO_alto"]))
	add(R, "C21", "VPH de triaje realizado tras citología con hallazgos", iv(r["CITO_trivph"]))
	add(R, "C23", "VPH de triaje positivo 16/18", iv(r["CITO_t1618"]))
	add(R, "C24", "VPH de triaje positivo otros AR", iv(r["CITO_totros"]))
	add(R, "C25", "Derivadas a colposcopia - citología", iv(r["CITO_deriv"]))
	add(R, "C26", "Colposcopia realizada adecuada - citología", iv(r["CITO_adec"]))
	add(R, "C28", "LSIL/CIN1 - citología", iv(r["CITO_h1"]))
	add(R, "C29", "HSIL/CIN2+ - citología", iv(r["CITO_h2"]))
	add(R, "C30", "Adenocarcinoma in situ - citología", iv(r["CITO_h3"]))
	add(R, "C31", "Cáncer invasivo - citología", iv(r["CITO_h4"]))
	for _, m := range []struct{ col, k, n string }{{"C", "VPH_PROF", "profesional"}, {"D", "VPH_AUTO", "autotoma"}} {
		add(R, m.col+"34", "Resultado válido VPH primario - "+m.n, iv(r[m.k+"_val"]))
		add(R, m.col+"36", "VPH 16/18 - "+m.n, iv(r[m.k+"_1618"]))
		add(R, m.col+"37", "VPH otros AR - "+m.n, iv(r[m.k+"_otros"]))
		add(R, m.col+"38", "Citología de triaje tras VPH otros AR - "+m.n, iv(r[m.k+"_tricito"]))
		add(R, m.col+"40", "Triaje bajo grado - "+m.n, iv(r[m.k+"_tbajo"]))
		add(R, m.col+"41", "Triaje alto grado (ASC-H/HSIL/AGC) - "+m.n, iv(r[m.k+"_talto"]))
		add(R, m.col+"42", "Triaje lesión severa (AIS/carcinoma) - "+m.n, iv(r[m.k+"_tsev"]))
		add(R, m.col+"43", "Derivadas a colposcopia - "+m.n, iv(r[m.k+"_deriv"]))
		add(R, m.col+"44", "Colposcopia realizada adecuada - "+m.n, iv(r[m.k+"_adec"]))
		add(R, m.col+"46", "LSIL/CIN1 - "+m.n, iv(r[m.k+"_h1"]))
		add(R, m.col+"47", "HSIL/CIN2+ - "+m.n, iv(r[m.k+"_h2"]))
		add(R, m.col+"48", "Adenocarcinoma in situ - "+m.n, iv(r[m.k+"_h3"]))
		add(R, m.col+"49", "Cáncer invasivo - "+m.n, iv(r[m.k+"_h4"]))
	}

	// ---------------- Indicadores derivados ----------------
	pct := func(a, b int) string {
		if b == 0 {
			return "-"
		}
		return fmt.Sprintf("%.1f %%", 100*float64(a)/float64(b))
	}
	por1000 := func(a, b int) string {
		if b == 0 {
			return "-"
		}
		return fmt.Sprintf("%.1f ‰", 1000*float64(a)/float64(b))
	}
	vVal := r["VPH_PROF_val"] + r["VPH_AUTO_val"]
	vPos := r["VPH_PROF_1618"] + r["VPH_PROF_otros"] + r["VPH_AUTO_1618"] + r["VPH_AUTO_otros"]
	vDer := r["VPH_PROF_deriv"] + r["VPH_AUTO_deriv"]
	vAde := r["VPH_PROF_adec"] + r["VPH_AUTO_adec"]
	vCIN2 := 0
	for _, k := range []string{"VPH_PROF", "VPH_AUTO"} {
		vCIN2 += r[k+"_h2"] + r[k+"_h3"] + r[k+"_h4"]
	}
	cCIN2 := r["CITO_h2"] + r["CITO_h3"] + r["CITO_h4"]
	intC := b.pInt("intervalo_cito_meses") / 12
	intV := b.pInt("intervalo_vph_meses") / 12
	ind := func(n, v, f string) { inf.Indicadores = append(inf.Indicadores, [3]string{n, v, f}) }
	ind("Extensión de invitación - citología", pct(p["CITO_inv"], (pobC-excC)/max(intC, 1)), fmt.Sprintf("C15 / ((C5-C6)/%d)", intC))
	ind("Extensión de invitación - VPH", pct(p["VPH_PROF_inv"]+p["VPH_AUTO_inv"], (pobV-excV)/max(intV, 1)), fmt.Sprintf("(C21+D21) / ((D5-D6)/%d)", intV))
	ind("Participación - citología", pct(p["CITO_res"], p["CITO_inv"]-p["CITO_novalida"]), "C18 / (C15-C16)")
	ind("Participación - VPH profesional", pct(p["VPH_PROF_res"], p["VPH_PROF_inv"]-p["VPH_PROF_novalida"]), "C24 / (C21-C22)")
	ind("Participación - VPH autotoma", pct(p["VPH_AUTO_res"], p["VPH_AUTO_inv"]-p["VPH_AUTO_novalida"]), "D24 / (D21-D22)")
	ind("Citologías anormales", pct(r["CITO_bajo"]+r["CITO_alto"], r["CITO_val"]), "(C19+C20) / C17")
	ind("Positividad VPH-AR", pct(vPos, vVal), "(C36+C37+D36+D37) / (C34+D34)")
	ind("Derivación a colposcopia - citología", pct(r["CITO_deriv"], r["CITO_val"]), "C25 / C17")
	ind("Derivación a colposcopia - VPH", pct(vDer, vVal), "(C43+D43) / (C34+D34)")
	ind("Colposcopia realizada adecuada - citología", pct(r["CITO_adec"], r["CITO_deriv"]), "C26 / C25")
	ind("Colposcopia realizada adecuada - VPH", pct(vAde, vDer), "(C44+D44) / (C43+D43)")
	ind("Tasa de detección CIN2+ - citología", por1000(cCIN2, r["CITO_val"]), "(C29+C30+C31) / C17")
	ind("Tasa de detección CIN2+ - VPH", por1000(vCIN2, vVal), "(C47+C48+C49+D47+D48+D49) / (C34+D34)")
	ind("VPP de la derivación para CIN2+ - VPH", pct(vCIN2, vDer), "CIN2+ / (C43+D43)")
	if len(dias) > 0 {
		sort.Ints(dias)
		le := 0
		for _, d := range dias {
			if d <= 60 {
				le++
			}
		}
		ind("Derivación → colposcopia: mediana (días)", fmt.Sprint(dias[len(dias)/2]), "colposcopias realizadas de la cohorte")
		ind("Derivación → colposcopia: percentil 90 (días)", fmt.Sprint(dias[len(dias)*9/10]), "")
		ind("Colposcopias en ≤ 60 días", pct(le, len(dias)), "")
	}

	// ---------------- Comprobaciones ----------------
	chk := func(n string, ok bool, det string) {
		s := "OK"
		if !ok {
			s = "REVISAR"
		}
		inf.Controles = append(inf.Controles, [2]string{n, s + " " + det})
	}
	chk("C18 ≤ C15 − C16", p["CITO_res"] <= p["CITO_inv"]-p["CITO_novalida"], "")
	chk("C24 ≤ C21 − C22 y D24 ≤ D21 − D22", p["VPH_PROF_res"] <= p["VPH_PROF_inv"]-p["VPH_PROF_novalida"] && p["VPH_AUTO_res"] <= p["VPH_AUTO_inv"]-p["VPH_AUTO_novalida"], "")
	chk("C19 + C20 ≤ C17", r["CITO_bajo"]+r["CITO_alto"] <= r["CITO_val"], "")
	chk("C23 + C24 ≤ C21", r["CITO_t1618"]+r["CITO_totros"] <= r["CITO_trivph"], "")
	for _, k := range []string{"VPH_PROF", "VPH_AUTO"} {
		chk(k+": 16/18 + otros ≤ válidos", r[k+"_1618"]+r[k+"_otros"] <= r[k+"_val"], "")
		chk(k+": triaje bajo+alto+severo ≤ triajes realizados", r[k+"_tbajo"]+r[k+"_talto"]+r[k+"_tsev"] <= r[k+"_tricito"], "")
		chk(k+": adecuadas ≤ derivadas", r[k+"_adec"] <= r[k+"_deriv"], "")
	}
	chk("C26 ≤ C25", r["CITO_adec"] <= r["CITO_deriv"], "")
	nOtro := 0
	rows, err = b.Query(`SELECT COUNT(*) FROM invitaciones WHERE fecha_invitacion BETWEEN ? AND ? AND (edad < ? OR edad > ?)`, fs(ini), fs(fin), citoMin, vphMax)
	if err == nil {
		if rows.Next() {
			rows.Scan(&nOtro)
		}
		rows.Close()
	}
	chk(fmt.Sprintf("Ninguna invitación fuera de %d-%d años", citoMin, vphMax), nOtro == 0, fmt.Sprintf("(%d)", nOtro))
	return inf, nil
}

func (inf *Informe) markdown(w io.Writer) {
	fmt.Fprintf(w, "# PDPCX · KPIs %d (plantilla de la Red)\n\n", inf.Anio)
	fmt.Fprintf(w, "Generado %s. Personas, no invitaciones ni pruebas.\n\n", time.Now().Format("2006-01-02 15:04"))
	for _, h := range []string{"Participación", "Resultados"} {
		if h == "Participación" {
			fmt.Fprintf(w, "## Pestaña Participación — mujeres invitadas en %d\n\n", inf.Anio)
		} else {
			fmt.Fprintf(w, "## Pestaña Resultados — mujeres con muestra primaria tomada en %d\n\n", inf.Anio)
		}
		fmt.Fprintln(w, "| Celda | Variable | Valor |")
		fmt.Fprintln(w, "|---|---|---:|")
		for _, c := range inf.Celdas {
			if c.Hoja != h {
				continue
			}
			v := "PENDIENTE (parámetro INE)"
			if c.Valor != nil {
				v = fmt.Sprint(*c.Valor)
			}
			fmt.Fprintf(w, "| %s | %s | %s |\n", c.Celda, c.Desc, v)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprint(w, "## Indicadores derivados\n\n")
	fmt.Fprintln(w, "| Indicador | Valor | Cálculo |")
	fmt.Fprintln(w, "|---|---:|---|")
	for _, x := range inf.Indicadores {
		fmt.Fprintf(w, "| %s | %s | %s |\n", x[0], x[1], x[2])
	}
	fmt.Fprint(w, "\n## Comprobaciones de coherencia\n\n")
	fmt.Fprintln(w, "| Comprobación | Resultado |")
	fmt.Fprintln(w, "|---|---|")
	for _, x := range inf.Controles {
		fmt.Fprintf(w, "| %s | %s |\n", x[0], x[1])
	}
}

func (inf *Informe) csv(ruta string) error {
	var filas [][]string
	for _, c := range inf.Celdas {
		v := ""
		if c.Valor != nil {
			v = fmt.Sprint(*c.Valor)
		}
		filas = append(filas, []string{fmt.Sprint(inf.Anio), c.Hoja, c.Celda, c.Desc, v})
	}
	return escribirCSV(ruta, []string{"anio", "hoja", "celda", "variable", "valor"}, filas)
}
