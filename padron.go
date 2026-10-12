package main

import (
	"database/sql"
	"encoding/csv"
	"fmt"
	"hash/fnv"
	"io"
	"math/rand"
	"os"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Lectura de CSV (separador ';', cabecera obligatoria, UTF-8)
// ---------------------------------------------------------------------------

type filaCSV map[string]string

func leerCSV(ruta string, obligatorias ...string) ([]filaCSV, error) {
	f, err := os.Open(ruta)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.Comma = ';'
	r.TrimLeadingSpace = true
	r.FieldsPerRecord = -1
	cab, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("%s: sin cabecera: %w", ruta, err)
	}
	for i := range cab {
		cab[i] = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(cab[i], "\xef\xbb\xbf")))
	}
	for _, o := range obligatorias {
		found := false
		for _, c := range cab {
			if c == o {
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("%s: falta la columna %q (cabecera: %s)", ruta, o, strings.Join(cab, ";"))
		}
	}
	var out []filaCSV
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		m := filaCSV{}
		for i, c := range cab {
			if i < len(rec) {
				m[c] = strings.TrimSpace(rec[i])
			}
		}
		out = append(out, m)
	}
	return out, nil
}

func escribirCSV(ruta string, cab []string, filas [][]string) error {
	f, err := os.Create(ruta)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	w.Comma = ';'
	if err := w.Write(cab); err != nil {
		return err
	}
	if err := w.WriteAll(filas); err != nil {
		return err
	}
	w.Flush()
	return w.Error()
}

// ---------------------------------------------------------------------------
// Carga de padrón / TIS
//   cip;nhc;fecha_nacimiento;zbs;municipio;cod_postal
// Es idempotente: una mujer que ya existe solo actualiza sus datos de contacto.
// ---------------------------------------------------------------------------

type resumenCarga struct {
	Nuevas, Actualizadas, Activas, Pendientes, FueraEdad, Errores int
}

func (pg Programa) cargarPadron(ruta string, fCarga time.Time, origen string) (resumenCarga, error) {
	var rs resumenCarga
	filas, err := leerCSV(ruta, "cip", "fecha_nacimiento")
	if err != nil {
		return rs, err
	}
	inicio, _ := pg.pFecha("fecha_inicio_programa")
	eMin, eMax := pg.pInt("edad_cito_min"), pg.pInt("edad_vph_max")

	tx, err := pg.Begin()
	if err != nil {
		return rs, err
	}
	defer tx.Rollback()
	sel, err := tx.Prepare(`SELECT id, COALESCE(nhc,''), fecha_nacimiento, COALESCE(zbs,''), COALESCE(municipio,''), COALESCE(cod_postal,'') FROM mujeres WHERE cip=?`)
	if err != nil {
		return rs, err
	}
	defer sel.Close()
	ins, err := tx.Prepare(`INSERT INTO mujeres(cip,nhc,fecha_nacimiento,zbs,municipio,cod_postal,estado,fecha_estado,fecha_proxima,motivo_proxima,fecha_alta,origen)
	                      VALUES(?,?,?,?,?,?,?,?,?,?,?,?) RETURNING id`)
	if err != nil {
		return rs, err
	}
	defer ins.Close()

	for n, f := range filas {
		fn, err := parseFecha(f["fecha_nacimiento"])
		if err != nil || f["cip"] == "" {
			rs.Errores++
			fmt.Fprintf(os.Stderr, "  fila %d rechazada: cip=%q fecha=%q\n", n+2, f["cip"], f["fecha_nacimiento"])
			continue
		}
		var id int64
		var act [5]string // nhc, fecha_nacimiento, zbs, municipio, cod_postal
		err = sel.QueryRow(f["cip"]).Scan(&id, &act[0], &act[1], &act[2], &act[3], &act[4])
		if err == nil {
			// Registro maestro: se guarda cada dato que cambia, con su valor anterior.
			campos := []string{"nhc", "fecha_nacimiento", "zbs", "municipio", "cod_postal"}
			nuevos := []string{f["nhc"], fs(fn), f["zbs"], f["municipio"], f["cod_postal"]}
			cambios := 0
			for i, c := range campos {
				if act[i] == nuevos[i] {
					continue
				}
				if _, err := tx.Exec(`UPDATE mujeres SET `+c+`=? WHERE id=?`, nuevos[i], id); err != nil {
					return rs, err
				}
				if _, err := tx.Exec(`INSERT INTO historial_persona(mujer_id,fecha,campo,anterior,nuevo,origen,usuario) VALUES(?,?,?,?,?,?,?)`,
					id, fs(fCarga), c, act[i], nuevos[i], origen, pg.Usuario); err != nil {
					return rs, err
				}
				cambios++
			}
			if cambios > 0 {
				if err := pg.ev(tx, fCarga, id, 0, "ACTUALIZACION_PADRON", fmt.Sprintf("%d dato(s) cambiado(s)", cambios), origen); err != nil {
					return rs, err
				}
				rs.Actualizadas++
			}
			continue
		}
		if err != sql.ErrNoRows {
			return rs, err
		}
		e := edad(fn, fCarga)
		estado, motivo := "ACTIVA", "RONDA_INICIAL"
		var prox any
		switch {
		case e < eMin:
			estado, motivo = "PENDIENTE_EDAD", ""
			rs.Pendientes++
		case e > eMax:
			rs.FueraEdad++
			continue // no entra en el programa
		default:
			prox = fs(fechaRondaInicial(f["cip"], fn, fCarga, inicio, pg.BD))
			rs.Activas++
		}
		var mot any
		if motivo != "" {
			mot = motivo
		}
		if err := ins.QueryRow(f["cip"], f["nhc"], fs(fn), f["zbs"], f["municipio"], f["cod_postal"], estado, fs(fCarga), prox, mot, fs(fCarga), origen).Scan(&id); err != nil {
			return rs, fmt.Errorf("fila %d: %w", n+2, err)
		}
		if err := pg.ev(tx, fCarga, id, 0, "ALTA_PADRON", estado, origen); err != nil {
			return rs, err
		}
		rs.Nuevas++
	}
	if err := pg.registrarCarga(tx, "PADRON", "ENTRADA", ruta, len(filas), fmt.Sprintf("%+v", rs)); err != nil {
		return rs, err
	}
	return rs, tx.Commit()
}

// fechaRondaInicial reparte la primera ronda a lo largo del intervalo de la
// prueba que le corresponde (3 años citología / 5 años VPH) para no invitar a
// toda la población el primer año. El reparto es determinista (hash del CIP).
// Las mujeres que cumplirían 66 antes de su turno se adelantan.
// Las altas posteriores al inicio del programa se citan en la fecha de carga.
func fechaRondaInicial(cip string, fn, fCarga, inicio time.Time, b *BD) time.Time {
	if fCarga.After(inicio.AddDate(0, 0, 31)) {
		return fCarga
	}
	base := maxT(inicio, fCarga)
	meses := b.pInt("intervalo_vph_meses")
	if edad(fn, base) <= b.pInt("edad_cito_max") {
		meses = b.pInt("intervalo_cito_meses")
	}
	fin := addMeses(base, meses)
	limite := cumple(fn, b.pInt("edad_vph_max")+1).AddDate(0, 0, -60)
	if limite.Before(fin) {
		fin = limite
	}
	dias := int(fin.Sub(base).Hours() / 24)
	if dias <= 0 {
		return base
	}
	h := fnv.New32a()
	h.Write([]byte(cip))
	return base.AddDate(0, 0, int(h.Sum32()%uint32(dias)))
}

// ---------------------------------------------------------------------------
// Alta anual: entra la cohorte que cumple 25 y salen las que han cumplido 66.
// ---------------------------------------------------------------------------

type resumenAlta struct{ Entran, Salen int }

func (pg Programa) altaAnual(anio int, origen string) (resumenAlta, error) {
	var rs resumenAlta
	ini := time.Date(anio, 1, 1, 0, 0, 0, 0, time.UTC)
	fin := time.Date(anio, 12, 31, 0, 0, 0, 0, time.UTC)
	eMin, eMax := pg.pInt("edad_cito_min"), pg.pInt("edad_vph_max")

	tx, err := pg.Begin()
	if err != nil {
		return rs, err
	}
	defer tx.Rollback()

	type m struct {
		id int64
		fn time.Time
	}
	leer := func(q string, args ...any) ([]m, error) {
		rows, err := tx.Query(q, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []m
		for rows.Next() {
			var x m
			var f string
			if err := rows.Scan(&x.id, &f); err != nil {
				return nil, err
			}
			x.fn = mustFecha(f)
			out = append(out, x)
		}
		return out, rows.Err()
	}

	// Entran: cumplen 25 durante el año (o antes, si se cargaron tarde).
	pend, err := leer(`SELECT id, fecha_nacimiento FROM mujeres WHERE estado='PENDIENTE_EDAD'`)
	if err != nil {
		return rs, err
	}
	for _, x := range pend {
		c := cumple(x.fn, eMin)
		if c.After(fin) {
			continue
		}
		prox := maxT(c, ini)
		if _, err := tx.Exec(`UPDATE mujeres SET estado='ACTIVA', fecha_estado=?, fecha_proxima=?, motivo_proxima='ENTRADA_25' WHERE id=?`,
			fs(prox), fs(prox), x.id); err != nil {
			return rs, err
		}
		if err := pg.ev(tx, prox, x.id, 0, "ENTRADA_PROGRAMA", fmt.Sprintf("cumple %d años", eMin), origen); err != nil {
			return rs, err
		}
		rs.Entran++
	}

	// Salen: han cumplido 66 antes del 1 de enero. Las que los cumplen durante
	// el año siguen siendo invitables hasta la víspera (el planificador lo
	// controla con la edad a la fecha de invitación).
	act, err := leer(`SELECT id, fecha_nacimiento FROM mujeres WHERE estado IN ('ACTIVA','SEGUIMIENTO_CLINICO')`)
	if err != nil {
		return rs, err
	}
	for _, x := range act {
		c := cumple(x.fn, eMax+1)
		if !c.Before(ini) {
			continue
		}
		if _, err := tx.Exec(`UPDATE mujeres SET estado='SALIDA_EDAD', fecha_estado=?, fecha_proxima=NULL, motivo_proxima=NULL WHERE id=?`,
			fs(c), x.id); err != nil {
			return rs, err
		}
		if err := pg.ev(tx, c, x.id, 0, "SALIDA_EDAD", fmt.Sprintf("cumple %d años", eMax+1), origen); err != nil {
			return rs, err
		}
		rs.Salen++
	}
	if err := pg.registrarCarga(tx, "ALTA_ANUAL", "PROCESO", fmt.Sprint(anio), rs.Entran+rs.Salen, fmt.Sprintf("%+v", rs)); err != nil {
		return rs, err
	}
	return rs, tx.Commit()
}

// ---------------------------------------------------------------------------
// Generador de padrón SINTÉTICO (para pruebas; no son personas reales)
// ---------------------------------------------------------------------------

// Pesos aproximados por grupo quinquenal (25-29 ... 60-64, 65). Ilustrativos.
var pesosEdad = []struct {
	desde, hasta int
	peso         float64
}{
	{25, 29, 0.090}, {30, 34, 0.095}, {35, 39, 0.105}, {40, 44, 0.130},
	{45, 49, 0.140}, {50, 54, 0.135}, {55, 59, 0.130}, {60, 64, 0.120}, {65, 65, 0.025},
}

func generarMujer(r *rand.Rand, fn time.Time, sec int, prefijo string, nhcBase int) []string {
	zbs := fmt.Sprintf("ZBS%02d", 1+r.Intn(20))
	mun := fmt.Sprintf("26%03d", 1+r.Intn(174))
	cp := fmt.Sprintf("26%03d", r.Intn(600))
	cip := fmt.Sprintf("%s%08d", prefijo, sec)
	nhc := fmt.Sprintf("%d", nhcBase+sec)
	return []string{cip, nhc, fs(fn), zbs, mun, cp}
}

func fechaNacAleatoria(r *rand.Rand, edadAl int, ref time.Time) time.Time {
	// nacida entre ref-(edad+1) años +1 día y ref-edad años
	hasta := ref.AddDate(-edadAl, 0, 0)
	return hasta.AddDate(0, 0, -r.Intn(365))
}

// generarPadron escribe n mujeres de 25 a 65 años a 01/01/anio.
func generarPadron(ruta string, n, anio int, semilla int64) error {
	r := rand.New(rand.NewSource(semilla))
	ref := time.Date(anio, 1, 1, 0, 0, 0, 0, time.UTC)
	var tot float64
	for _, p := range pesosEdad {
		tot += p.peso
	}
	filas := make([][]string, 0, n)
	for i := 0; i < n; i++ {
		x := r.Float64() * tot
		g := pesosEdad[len(pesosEdad)-1]
		for _, p := range pesosEdad {
			if x < p.peso {
				g = p
				break
			}
			x -= p.peso
		}
		e := g.desde + r.Intn(g.hasta-g.desde+1)
		filas = append(filas, generarMujer(r, fechaNacAleatoria(r, e, ref), i+1, "SINT", 1000000))
	}
	return escribirCSV(ruta, []string{"cip", "nhc", "fecha_nacimiento", "zbs", "municipio", "cod_postal"}, filas)
}

// generarCohorte escribe n mujeres que cumplen 25 años durante 'anio'.
func generarCohorte(ruta string, n, anio int, semilla int64) error {
	r := rand.New(rand.NewSource(semilla + int64(anio)))
	filas := make([][]string, 0, n)
	for i := 0; i < n; i++ {
		fn := time.Date(anio-25, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, r.Intn(365))
		filas = append(filas, generarMujer(r, fn, i+1, fmt.Sprintf("C%d", anio), anio*10000000))
	}
	return escribirCSV(ruta, []string{"cip", "nhc", "fecha_nacimiento", "zbs", "municipio", "cod_postal"}, filas)
}
