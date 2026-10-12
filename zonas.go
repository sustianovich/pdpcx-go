package main

import (
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Catálogo de zonas básicas de salud (ZBS)
//
// El programa no fija cuántas zonas hay: cada comunidad carga las suyas, a
// mano o por CSV. El código es el mismo que llega en la columna zbs del padrón.
// ---------------------------------------------------------------------------

type Zona struct {
	Codigo, Nombre, Area, Centro, Municipio, Notas string
	Cupo                                           sql.NullInt64 // invitaciones al mes; sin valor = sin tope
	Activa                                         bool
	Actualizada, Usuario                           string
	Mujeres, Activas                               int // mujeres del padrón en la zona (total y en estado ACTIVA)
}

// normZBS iguala los códigos escritos de formas distintas ("zbs01 " = "ZBS01").
func normZBS(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

// zonaDeCampos valida los datos de una zona tal como llegan de un formulario
// o de una fila CSV. Sin nombre se usa el código; sin "activa" se da por activa.
func zonaDeCampos(codigo, nombre, area, centro, municipio, cupo, activa, notas string) (Zona, error) {
	z := Zona{Codigo: normZBS(codigo), Nombre: strings.TrimSpace(nombre), Area: strings.TrimSpace(area),
		Centro: strings.TrimSpace(centro), Municipio: strings.TrimSpace(municipio), Notas: strings.TrimSpace(notas), Activa: true}
	if z.Codigo == "" {
		return z, fmt.Errorf("falta el código de la zona")
	}
	if len([]rune(z.Codigo)) > 40 || strings.ContainsAny(z.Codigo, ";\"\r\n\t") {
		return z, fmt.Errorf("código de zona no válido %q (máx. 40 caracteres, sin ; ni comillas)", z.Codigo)
	}
	if z.Nombre == "" {
		z.Nombre = z.Codigo
	}
	if c := strings.TrimSpace(cupo); c != "" {
		n, err := strconv.Atoi(c)
		if err != nil || n < 0 {
			return z, fmt.Errorf("cupo_mensual debe ser un entero no negativo o vacío: %q", c)
		}
		z.Cupo = sql.NullInt64{Int64: int64(n), Valid: true}
	}
	if a := strings.ToUpper(strings.TrimSpace(activa)); a != "" {
		switch a {
		case "S", "SI", "SÍ", "1", "TRUE":
		case "N", "NO", "0", "FALSE":
			z.Activa = false
		default:
			return z, fmt.Errorf("activa debe ser S o N: %q", activa)
		}
	}
	return z, nil
}

// guardarZona da de alta la zona o actualiza sus datos si el código ya existe.
func (pg Programa) guardarZona(x ejecutor, z Zona) error {
	act := 0
	if z.Activa {
		act = 1
	}
	nulo := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	_, err := x.Exec(`INSERT INTO zbs(codigo,nombre,area,centro,municipio,cupo_mensual,activa,notas,actualizada,usuario)
	                  VALUES(?,?,?,?,?,?,?,?,?,?)
	                  ON CONFLICT (codigo) DO UPDATE SET nombre=excluded.nombre, area=excluded.area, centro=excluded.centro,
	                      municipio=excluded.municipio, cupo_mensual=excluded.cupo_mensual, activa=excluded.activa,
	                      notas=excluded.notas, actualizada=excluded.actualizada, usuario=excluded.usuario`,
		z.Codigo, z.Nombre, nulo(z.Area), nulo(z.Centro), nulo(z.Municipio), z.Cupo, act, nulo(z.Notas), ahoraTS(), pg.Usuario)
	return err
}

// --- zonas: codigo;nombre;area;centro;municipio;cupo_mensual;activa;notas
func (pg Programa) importarZonas(ruta string) (resumenImport, error) {
	return pg.importar(ruta, "ZBS", []string{"codigo"},
		func(tx *Tx, f filaCSV) error {
			z, err := zonaDeCampos(f["codigo"], f["nombre"], f["area"], f["centro"], f["municipio"], f["cupo_mensual"], f["activa"], f["notas"])
			if err != nil {
				return err
			}
			return pg.guardarZona(tx, z)
		})
}

// mujeresPorZona cuenta las mujeres del padrón por código de zona normalizado
// (total y en estado ACTIVA). La clave "" reúne a las que no tienen zona.
func (b *BD) mujeresPorZona() (map[string][2]int, error) {
	rows, err := b.Query(`SELECT COALESCE(zbs,''), COUNT(*), SUM(CASE WHEN estado='ACTIVA' THEN 1 ELSE 0 END)
	                      FROM mujeres WHERE estado<>'FUSIONADA' GROUP BY COALESCE(zbs,'')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string][2]int{}
	for rows.Next() {
		var z string
		var n, a int
		if err := rows.Scan(&z, &n, &a); err != nil {
			return nil, err
		}
		k := normZBS(z)
		m[k] = [2]int{m[k][0] + n, m[k][1] + a}
	}
	return m, rows.Err()
}

// listarZonas devuelve el catálogo con las mujeres de cada zona, las zonas que
// aparecen en el padrón pero no están catalogadas y las mujeres sin zona.
func (b *BD) listarZonas() (cat, sinCatalogar []Zona, sinZona int, err error) {
	porZona, err := b.mujeresPorZona()
	if err != nil {
		return nil, nil, 0, err
	}
	rows, err := b.Query(`SELECT codigo, nombre, COALESCE(area,''), COALESCE(centro,''), COALESCE(municipio,''), cupo_mensual,
	                             activa, COALESCE(notas,''), actualizada, COALESCE(usuario,'') FROM zbs ORDER BY codigo`)
	if err != nil {
		return nil, nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var z Zona
		var act int
		if err := rows.Scan(&z.Codigo, &z.Nombre, &z.Area, &z.Centro, &z.Municipio, &z.Cupo, &act, &z.Notas, &z.Actualizada, &z.Usuario); err != nil {
			return nil, nil, 0, err
		}
		z.Activa = act == 1
		z.Mujeres, z.Activas = porZona[z.Codigo][0], porZona[z.Codigo][1]
		delete(porZona, z.Codigo)
		cat = append(cat, z)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, 0, err
	}
	sinZona = porZona[""][0]
	delete(porZona, "")
	for k, v := range porZona {
		sinCatalogar = append(sinCatalogar, Zona{Codigo: k, Nombre: k, Activa: true, Mujeres: v[0], Activas: v[1]})
	}
	sort.Slice(sinCatalogar, func(i, j int) bool { return sinCatalogar[i].Codigo < sinCatalogar[j].Codigo })
	return cat, sinCatalogar, sinZona, nil
}

func (b *BD) zonaPorCodigo(codigo string) (*Zona, error) {
	z := &Zona{}
	var act int
	err := b.QueryRow(`SELECT codigo, nombre, COALESCE(area,''), COALESCE(centro,''), COALESCE(municipio,''), cupo_mensual,
	                          activa, COALESCE(notas,'') FROM zbs WHERE codigo=?`, normZBS(codigo)).
		Scan(&z.Codigo, &z.Nombre, &z.Area, &z.Centro, &z.Municipio, &z.Cupo, &act, &z.Notas)
	z.Activa = act == 1
	return z, err
}

// altaZonasDelPadron cataloga las zonas que ya vienen en el padrón y aún no
// están en el catálogo (con el código como nombre y sin cupo). No toca las demás.
func (pg Programa) altaZonasDelPadron() (int, error) {
	_, faltan, _, err := pg.listarZonas()
	if err != nil {
		return 0, err
	}
	tx, err := pg.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n := 0
	for _, z := range faltan {
		if _, err := zonaDeCampos(z.Codigo, "", "", "", "", "", "", ""); err != nil {
			continue // código del padrón no catalogable: se queda en la lista de avisos
		}
		res, err := tx.Exec(`INSERT INTO zbs(codigo,nombre,activa,actualizada,usuario) VALUES(?,?,1,?,?) ON CONFLICT (codigo) DO NOTHING`,
			z.Codigo, z.Codigo, ahoraTS(), pg.Usuario)
		if err != nil {
			return 0, err
		}
		if k, _ := res.RowsAffected(); k > 0 {
			n++
		}
	}
	return n, tx.Commit()
}

var cabZonas = []string{"codigo", "nombre", "area", "centro", "municipio", "cupo_mensual", "activa", "notas"}

// filasZonas: el catálogo en el mismo formato que se importa.
func filasZonas(zs []Zona) [][]string {
	var filas [][]string
	for _, z := range zs {
		cupo, act := "", "S"
		if z.Cupo.Valid {
			cupo = fmt.Sprint(z.Cupo.Int64)
		}
		if !z.Activa {
			act = "N"
		}
		filas = append(filas, []string{z.Codigo, z.Nombre, z.Area, z.Centro, z.Municipio, cupo, act, z.Notas})
	}
	return filas
}
