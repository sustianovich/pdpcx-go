package main

import (
	"database/sql"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Ficha completa de una mujer (la usan la orden "trazar" y la web).

type ExclF struct{ Tipo, Motivo, Inicio, Fin string }
type VacF struct {
	Fecha, Vacuna string
	Dosis         sql.NullInt64
}
type HistF struct{ Fecha, Campo, Anterior, Nuevo, Origen, Usuario string }
type PrF struct{ Tipo, Finalidad, Muestra, Toma, Resultado, Valor string }
type ColF struct {
	ID                                                  int64
	Derivacion, Motivo, Estado, Realizacion, Histologia string
	Adecuada, Biopsia                                   int
}
type DecF struct {
	Fecha, Accion, Motivo, Protocolo, Usuario string
	Version                                   int64
}
type InvF struct {
	ID                                                                         int64
	Tipo, Fecha, Prueba, Muestra, Motivo, Causa, Estado, Cierre, FCierre, Lote string
	Ronda, Edad, Valida                                                        int
	Version                                                                    sql.NullInt64
	Pruebas                                                                    []PrF
	Colpos                                                                     []ColF
	Decisiones                                                                 []DecF
}
type EvF struct {
	Fecha, Tipo, Detalle, Origen, Usuario, Registrado string
	Inv                                               int64
}
type FichaMujer struct {
	ID                                                                     int64
	CIP, NHC, FechaNac, ZBS, Municipio, CP, Estado, FechaEstado, FechaAlta string
	Proxima, MotivoProx                                                    string
	FusionadaEn                                                            sql.NullInt64
	Exclusiones                                                            []ExclF
	Vacunas                                                                []VacF
	Historial                                                              []HistF
	Invitaciones                                                           []InvF
	Eventos                                                                []EvF
}

// buscarMujer acepta CIP, NHC o id interno.
func (b *BD) buscarMujer(clave string) (int64, error) {
	clave = strings.TrimSpace(clave)
	var id int64
	err := b.QueryRow(`SELECT id FROM mujeres WHERE cip=?`, clave).Scan(&id)
	if err == sql.ErrNoRows {
		err = b.QueryRow(`SELECT id FROM mujeres WHERE nhc=? ORDER BY id LIMIT 1`, clave).Scan(&id)
	}
	if err == sql.ErrNoRows {
		if n, e2 := strconv.ParseInt(clave, 10, 64); e2 == nil {
			err = b.QueryRow(`SELECT id FROM mujeres WHERE id=?`, n).Scan(&id)
		}
	}
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("no existe ninguna mujer con CIP/NHC/id %q", clave)
	}
	return id, err
}

func (b *BD) cargarFicha(id int64) (*FichaMujer, error) {
	f := &FichaMujer{ID: id}
	var prox, mot sql.NullString
	err := b.QueryRow(`SELECT cip, COALESCE(nhc,''), fecha_nacimiento, COALESCE(zbs,''), COALESCE(municipio,''), COALESCE(cod_postal,''),
	                          estado, fecha_estado, fecha_proxima, motivo_proxima, fecha_alta, fusionada_en
	                   FROM mujeres WHERE id=?`, id).
		Scan(&f.CIP, &f.NHC, &f.FechaNac, &f.ZBS, &f.Municipio, &f.CP, &f.Estado, &f.FechaEstado, &prox, &mot, &f.FechaAlta, &f.FusionadaEn)
	if err != nil {
		return nil, err
	}
	f.Proxima, f.MotivoProx = prox.String, mot.String

	q := func(s string, scan func(r *sql.Rows) error, args ...any) error {
		rows, err := b.Query(s, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if err := scan(rows); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	if err := q(`SELECT tipo, motivo, fecha_inicio, COALESCE(fecha_fin,'') FROM exclusiones WHERE mujer_id=? ORDER BY fecha_inicio`, func(r *sql.Rows) error {
		var x ExclF
		err := r.Scan(&x.Tipo, &x.Motivo, &x.Inicio, &x.Fin)
		f.Exclusiones = append(f.Exclusiones, x)
		return err
	}, id); err != nil {
		return nil, err
	}
	if err := q(`SELECT fecha, dosis, COALESCE(vacuna,'') FROM vacunacion_vph WHERE mujer_id=? ORDER BY fecha`, func(r *sql.Rows) error {
		var x VacF
		err := r.Scan(&x.Fecha, &x.Dosis, &x.Vacuna)
		f.Vacunas = append(f.Vacunas, x)
		return err
	}, id); err != nil {
		return nil, err
	}
	if err := q(`SELECT fecha, campo, COALESCE(anterior,''), COALESCE(nuevo,''), COALESCE(origen,''), COALESCE(usuario,'') FROM historial_persona WHERE mujer_id=? ORDER BY id`, func(r *sql.Rows) error {
		var x HistF
		err := r.Scan(&x.Fecha, &x.Campo, &x.Anterior, &x.Nuevo, &x.Origen, &x.Usuario)
		f.Historial = append(f.Historial, x)
		return err
	}, id); err != nil {
		return nil, err
	}
	if err := q(`SELECT id, tipo, ronda, fecha_invitacion, edad, prueba, muestra, motivo, valida, COALESCE(causa_no_valida,''),
	                    estado, COALESCE(motivo_cierre,''), COALESCE(fecha_cierre,''), COALESCE(lote,''), version_reglas
	             FROM invitaciones WHERE mujer_id=? ORDER BY fecha_invitacion, id`, func(r *sql.Rows) error {
		var x InvF
		err := r.Scan(&x.ID, &x.Tipo, &x.Ronda, &x.Fecha, &x.Edad, &x.Prueba, &x.Muestra, &x.Motivo, &x.Valida, &x.Causa,
			&x.Estado, &x.Cierre, &x.FCierre, &x.Lote, &x.Version)
		f.Invitaciones = append(f.Invitaciones, x)
		return err
	}, id); err != nil {
		return nil, err
	}
	for i := range f.Invitaciones {
		x := &f.Invitaciones[i]
		if err := q(`SELECT tipo, finalidad, muestra, fecha_toma, COALESCE(fecha_resultado,''), resultado FROM pruebas WHERE invitacion_id=? ORDER BY fecha_toma, id`, func(r *sql.Rows) error {
			var p PrF
			err := r.Scan(&p.Tipo, &p.Finalidad, &p.Muestra, &p.Toma, &p.Resultado, &p.Valor)
			x.Pruebas = append(x.Pruebas, p)
			return err
		}, x.ID); err != nil {
			return nil, err
		}
		if err := q(`SELECT id, fecha_derivacion, motivo_derivacion, estado, COALESCE(fecha_realizacion,''), COALESCE(adecuada,-1), COALESCE(biopsia,-1), COALESCE(histologia,'')
		             FROM colposcopias WHERE invitacion_id=? ORDER BY id`, func(r *sql.Rows) error {
			var c ColF
			err := r.Scan(&c.ID, &c.Derivacion, &c.Motivo, &c.Estado, &c.Realizacion, &c.Adecuada, &c.Biopsia, &c.Histologia)
			x.Colpos = append(x.Colpos, c)
			return err
		}, x.ID); err != nil {
			return nil, err
		}
		if err := q(`SELECT fecha, accion, COALESCE(motivo,''), version_reglas, protocolo, COALESCE(usuario,'') FROM decisiones WHERE invitacion_id=? ORDER BY id`, func(r *sql.Rows) error {
			var d DecF
			err := r.Scan(&d.Fecha, &d.Accion, &d.Motivo, &d.Version, &d.Protocolo, &d.Usuario)
			x.Decisiones = append(x.Decisiones, d)
			return err
		}, x.ID); err != nil {
			return nil, err
		}
	}
	if err := q(`SELECT fecha, COALESCE(invitacion_id,0), tipo, COALESCE(detalle,''), COALESCE(origen,''), COALESCE(usuario,''), registrado
	             FROM eventos WHERE mujer_id=? ORDER BY fecha, id`, func(r *sql.Rows) error {
		var e EvF
		err := r.Scan(&e.Fecha, &e.Inv, &e.Tipo, &e.Detalle, &e.Origen, &e.Usuario, &e.Registrado)
		f.Eventos = append(f.Eventos, e)
		return err
	}, id); err != nil {
		return nil, err
	}
	return f, nil
}

// InvAbierta devuelve la invitación no cerrada (para las acciones de la web).
func (f *FichaMujer) InvAbierta() *InvF {
	for i := len(f.Invitaciones) - 1; i >= 0; i-- {
		if f.Invitaciones[i].Estado != "CERRADA" {
			return &f.Invitaciones[i]
		}
	}
	return nil
}

func (f *FichaMujer) markdown(w io.Writer) {
	fmt.Fprintf(w, "# Traza de la mujer %s (NHC %s, id %d)\n\n", f.CIP, f.NHC, f.ID)
	fmt.Fprintf(w, "- Nacimiento: %s · ZBS: %s · Alta en el programa: %s\n", f.FechaNac, f.ZBS, f.FechaAlta)
	fmt.Fprintf(w, "- Estado: **%s** desde %s\n", f.Estado, f.FechaEstado)
	if f.FusionadaEn.Valid {
		fmt.Fprintf(w, "- Registro fusionado en la mujer id %d\n", f.FusionadaEn.Int64)
	}
	if f.Proxima != "" {
		fmt.Fprintf(w, "- Próxima cita prevista: %s (%s)\n", f.Proxima, f.MotivoProx)
	}
	if len(f.Exclusiones) > 0 {
		fmt.Fprint(w, "\n## Exclusiones\n\n")
		for _, x := range f.Exclusiones {
			fin := x.Fin
			if fin == "" {
				fin = "sin fin"
			}
			fmt.Fprintf(w, "- %s %s: %s → %s\n", x.Tipo, x.Motivo, x.Inicio, fin)
		}
	}
	if len(f.Vacunas) > 0 {
		fmt.Fprint(w, "\n## Vacunación VPH\n\n")
		for _, v := range f.Vacunas {
			fmt.Fprintf(w, "- %s dosis %v %s\n", v.Fecha, v.Dosis.Int64, v.Vacuna)
		}
	}
	if len(f.Historial) > 0 {
		fmt.Fprint(w, "\n## Historial de datos personales\n\n")
		for _, h := range f.Historial {
			fmt.Fprintf(w, "- %s %s: %q → %q (%s, %s)\n", h.Fecha, h.Campo, h.Anterior, h.Nuevo, h.Origen, h.Usuario)
		}
	}
	fmt.Fprint(w, "\n## Invitaciones\n\n")
	if len(f.Invitaciones) == 0 {
		fmt.Fprintln(w, "(ninguna)")
	}
	for _, x := range f.Invitaciones {
		val := "válida"
		if x.Valida == 0 {
			val = "NO válida: " + x.Causa
		}
		fmt.Fprintf(w, "### Invitación %d · %s ronda %d · %s · %d años\n", x.ID, x.Tipo, x.Ronda, x.Fecha, x.Edad)
		fmt.Fprintf(w, "- %s, muestra %s, motivo %s, %s, reglas v%d\n", x.Prueba, x.Muestra, x.Motivo, val, x.Version.Int64)
		est := x.Estado
		if x.Estado == "CERRADA" {
			est += " " + x.FCierre + " (" + x.Cierre + ")"
		}
		fmt.Fprintf(w, "- Estado: %s\n", est)
		for _, p := range x.Pruebas {
			fmt.Fprintf(w, "  - Prueba %s %s (%s): toma %s, resultado %s → **%s**\n", p.Tipo, p.Finalidad, p.Muestra, p.Toma, p.Resultado, p.Valor)
		}
		for _, c := range x.Colpos {
			fmt.Fprintf(w, "  - Colposcopia: derivada %s (%s) → %s %s adecuada=%v histología %s\n", c.Derivacion, c.Motivo, c.Estado, c.Realizacion, c.Adecuada == 1, c.Histologia)
		}
		for _, d := range x.Decisiones {
			fmt.Fprintf(w, "  - Decisión %s: %s (%s) · reglas v%d · %s\n", d.Fecha, d.Accion, d.Motivo, d.Version, d.Usuario)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprint(w, "## Eventos (traza completa)\n\n")
	fmt.Fprintln(w, "| Fecha | Invitación | Evento | Detalle | Origen | Usuario | Registrado |")
	fmt.Fprintln(w, "|---|---|---|---|---|---|---|")
	for _, e := range f.Eventos {
		inv := ""
		if e.Inv > 0 {
			inv = fmt.Sprint(e.Inv)
		}
		fmt.Fprintf(w, "| %s | %s | %s | %s | %s | %s | %s |\n", e.Fecha, inv, e.Tipo, strings.ReplaceAll(e.Detalle, "|", "/"), e.Origen, e.Usuario, e.Registrado)
	}
}

type ResumenBD struct {
	PorEstado    [][2]string
	Invitaciones [][6]string
	Cargas       [][5]string
}

func (b *BD) resumen() (*ResumenBD, error) {
	r := &ResumenBD{}
	rows, err := b.Query(`SELECT estado, COUNT(*) FROM mujeres GROUP BY estado ORDER BY 2 DESC`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var e string
		var n int
		rows.Scan(&e, &n)
		r.PorEstado = append(r.PorEstado, [2]string{e, fmt.Sprint(n)})
	}
	rows.Close()
	rows, err = b.Query(`SELECT substr(fecha_invitacion,1,4), tipo, prueba, muestra, COUNT(*), SUM(CASE WHEN estado<>'CERRADA' THEN 1 ELSE 0 END)
	                     FROM invitaciones GROUP BY substr(fecha_invitacion,1,4), tipo, prueba, muestra ORDER BY 1,2,3,4`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a, t, p, m string
		var n, ab int
		rows.Scan(&a, &t, &p, &m, &n, &ab)
		r.Invitaciones = append(r.Invitaciones, [6]string{a, t, p, m, fmt.Sprint(n), fmt.Sprint(ab)})
	}
	rows.Close()
	rows, err = b.Query(`SELECT fecha, tipo, sentido, COALESCE(fichero,''), COALESCE(filas,0) FROM cargas ORDER BY id DESC LIMIT 10`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f, t, s, fi string
		var n int
		rows.Scan(&f, &t, &s, &fi, &n)
		r.Cargas = append(r.Cargas, [5]string{f, t, s, fi, fmt.Sprint(n)})
	}
	return r, nil
}

func (b *BD) estado(w io.Writer) error {
	r, err := b.resumen()
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Reglas vigentes: versión %d · %s\n", b.verReglas(), protocoloVersion)
	fmt.Fprintln(w, "Mujeres por estado:")
	for _, x := range r.PorEstado {
		fmt.Fprintf(w, "  %-22s %8s\n", x[0], x[1])
	}
	fmt.Fprintln(w, "Invitaciones por año, tipo y prueba:")
	for _, x := range r.Invitaciones {
		fmt.Fprintf(w, "  %s %-8s %-10s %-4s %8s  (abiertas %s)\n", x[0], x[1], x[2], x[3], x[4], x[5])
	}
	fmt.Fprintln(w, "Últimos intercambios:")
	for _, x := range r.Cargas {
		fmt.Fprintf(w, "  %s %-18s %-8s %-30s %s\n", x[0], x[1], x[2], x[3], x[4])
	}
	return nil
}
