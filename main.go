// pdpcx: gestión del Programa de Detección Precoz de Cáncer de Cérvix.
//
// Citología 25-29 años, VPH-AR 30-65 (incluido), autotoma configurable.
// Base de datos SQLite (fichero) o PostgreSQL (servidor). Ver README.md.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const ayuda = `pdpcx — Programa de Detección Precoz de Cáncer de Cérvix

Uso: pdpcx <orden> [opciones]
  Todas las órdenes aceptan:
    -bd fichero.db  |  -bd "postgres://usuario:clave@servidor:5432/pdpcx?sslmode=require"
    -usuario nombre    (por defecto PDPCX_USUARIO o el usuario del sistema; queda en auditoría)

Configuración y reglas
  init                                     crea la base de datos y los parámetros por defecto
  parametro [clave valor -motivo "..."]    lista o cambia un parámetro (crea una versión de reglas)
  versiones                                historial de versiones de reglas

Usuarios y servicio web
  usuario-crear -login -nombre -rol        crea un usuario (clave en PDPCX_CLAVE o se pide)
  usuario-clave -login                     restablece la clave (deberá cambiarla al entrar)
  usuarios                                 lista usuarios
  servidor -escucha 127.0.0.1:8080 [-cert c.pem -key k.pem] [-cookie-segura] [-proxy]

Población (registro maestro)
  generar-padron  -n -anio -salida      padrón SINTÉTICO de mujeres 25-65 (pruebas)
  generar-cohorte -n -anio -salida      cohorte SINTÉTICA que cumple 25 en el año
  cargar-padron   -csv -fecha           alta/actualización (cip;nhc;fecha_nacimiento;zbs;municipio;cod_postal)
  alta-anual      -anio                 entran las que cumplen 25, salen las que han cumplido 66
  importar-bajas | importar-exclusiones | importar-vacunacion  -csv
  importar-zbs    -csv                  catálogo de zonas (codigo;nombre;area;centro;municipio;cupo_mensual;activa;notas)
  zonas           [-salida fichero]     lista el catálogo de zonas o lo exporta a CSV
  fusionar        -cip PRINCIPAL -duplicado CIP -motivo "..."
  reincorporar    -cip -fecha -motivo

Invitaciones
  planificar      -desde -hasta -lote   genera las invitaciones que tocan en el periodo
  exportar-lote   -lote -salida         CSV para imprenta/SMS (queda registrado como SALIDA)
  importar-no-validas -csv              cartas devueltas (invitacion_id;fecha;causa)
  cerrar-sin-respuesta -fecha           cierra invitaciones sin respuesta pasado el plazo

Resultados
  importar-pruebas      -csv            (cip;invitacion_id;tipo;finalidad;muestra;fecha_toma;fecha_resultado;resultado)
  importar-colposcopias -csv            (cip;invitacion_id;estado;fecha;adecuada;biopsia;histologia;fecha_histologia)

Explotación
  pendientes [-servicio ADMISION|AP|LABORATORIO|GINECOLOGIA|COORDINACION] [-fecha]   lista viva
  kpis    -anio [-md fichero] [-csv-salida fichero]
  trazar  -id <CIP|NHC|id>             historia completa de una mujer (queda en auditoría)
  estado

Pruebas
  simular -n 10000 -desde 2024 -hasta 2026 [-semilla 42] [-autotoma 2025-07-01]
`

func usuarioPorDefecto() string {
	if u := os.Getenv("PDPCX_USUARIO"); u != "" {
		return u
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "desconocido"
}

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "ayuda" || os.Args[1] == "--help" {
		fmt.Print(ayuda)
		return
	}
	orden := os.Args[1]
	fsx := flag.NewFlagSet(orden, flag.ExitOnError)
	rutaBD := fsx.String("bd", envODefecto("PDPCX_BD", "pdpcx.db"), "fichero SQLite o URL postgres://")
	quien := fsx.String("usuario", usuarioPorDefecto(), "usuario que ejecuta la orden (auditoría)")
	n := fsx.Int("n", 10000, "número de mujeres")
	anio := fsx.Int("anio", time.Now().Year(), "año")
	salida := fsx.String("salida", "", "fichero de salida")
	csvIn := fsx.String("csv", "", "fichero CSV de entrada (separador ;)")
	fecha := fsx.String("fecha", "", "fecha AAAA-MM-DD")
	desde := fsx.String("desde", "", "fecha o año inicial")
	hasta := fsx.String("hasta", "", "fecha o año final")
	lote := fsx.String("lote", "", "identificador de lote")
	cip := fsx.String("cip", "", "CIP de la mujer")
	dup := fsx.String("duplicado", "", "CIP duplicado a fusionar")
	id := fsx.String("id", "", "CIP, NHC o id interno")
	motivo := fsx.String("motivo", "", "motivo (obligatorio en cambios)")
	servicio := fsx.String("servicio", "", "servicio de la lista viva")
	semilla := fsx.Int64("semilla", 42, "semilla aleatoria (simulación)")
	md := fsx.String("md", "", "informe Markdown de salida")
	csvOut := fsx.String("csv-salida", "", "CSV de salida")
	autotoma := fsx.String("autotoma", "", "fecha de inicio de la autotoma (simulación)")
	estrategia := fsx.String("estrategia", "", "estrategia de autotoma: TODAS | NO_RESPONDEDORAS")
	login := fsx.String("login", "", "login de usuario")
	nombre := fsx.String("nombre", "", "nombre completo")
	rol := fsx.String("rol", "", "rol: "+strings.Join(roles, ", "))
	escucha := fsx.String("escucha", "127.0.0.1:8080", "dirección del servidor web")
	cert := fsx.String("cert", "", "certificado TLS (PEM)")
	key := fsx.String("key", "", "clave privada TLS (PEM)")
	cookieSegura := fsx.Bool("cookie-segura", false, "marcar la cookie como Secure (detrás de un proxy HTTPS)")
	proxy := fsx.Bool("proxy", false, "confiar en X-Forwarded-For (solo detrás de un proxy propio)")
	fsx.Parse(os.Args[2:])

	falla := func(err error) {
		if err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			os.Exit(1)
		}
	}
	fechaObl := func(s, nombre string) time.Time {
		if s == "" {
			falla(fmt.Errorf("falta -%s", nombre))
		}
		t, err := parseFecha(s)
		falla(err)
		return t
	}
	abrir := func() *BD {
		b, err := abrirBD(*rutaBD)
		falla(err)
		return b
	}
	prog := func(b *BD) Programa { return Programa{BD: b, Usuario: *quien, Canal: "CLI"} }
	audCLI := func(b *BD, accion, objeto, detalle string) { b.auditar(*quien, "", "CLI", accion, objeto, detalle, "") }

	switch orden {
	case "init":
		b := abrir()
		fmt.Printf("Base de datos lista (%s). Reglas v%d.\n", motor(b), b.verReglas())
		audCLI(b, "INIT", *rutaBD, "")
		b.Close()

	case "parametro":
		b := abrir()
		defer b.Close()
		args := fsx.Args()
		if len(args) >= 2 {
			falla(b.fijarParametro(args[0], args[1], *quien, "CLI", *motivo))
			fmt.Printf("%s = %s · reglas v%d\n", args[0], args[1], b.verReglas())
			return
		}
		rows, err := b.Query(`SELECT clave, valor, COALESCE(descripcion,'') FROM parametros ORDER BY clave`)
		falla(err)
		for rows.Next() {
			var k, v, d string
			rows.Scan(&k, &v, &d)
			fmt.Printf("%-30s %-18s %s\n", k, v, d)
		}
		rows.Close()
		fmt.Printf("Reglas vigentes: v%d · %s\n", b.verReglas(), protocoloVersion)

	case "versiones":
		b := abrir()
		defer b.Close()
		rows, err := b.Query(`SELECT id, creada, usuario, motivo FROM reglas_versiones ORDER BY id`)
		falla(err)
		for rows.Next() {
			var i int64
			var c, u, m string
			rows.Scan(&i, &c, &u, &m)
			fmt.Printf("v%-4d %s %-12s %s\n", i, c, u, m)
		}
		rows.Close()

	case "usuario-crear":
		b := abrir()
		defer b.Close()
		falla(b.crearUsuario(*login, *nombre, strings.ToUpper(*rol), leerClave(), *quien, "CLI"))
		fmt.Printf("Usuario %s creado con rol %s. Deberá cambiar la contraseña en su primer acceso.\n", *login, strings.ToUpper(*rol))

	case "usuario-clave":
		b := abrir()
		defer b.Close()
		var uid int64
		falla(b.QueryRow(`SELECT id FROM usuarios WHERE login=?`, strings.ToLower(*login)).Scan(&uid))
		falla(b.cambiarClave(uid, leerClave(), true, *quien, "CLI"))
		fmt.Println("Contraseña restablecida; se han cerrado sus sesiones.")

	case "usuarios":
		b := abrir()
		defer b.Close()
		us, err := b.listarUsuarios()
		falla(err)
		for _, u := range us {
			fmt.Printf("%-16s %-14s activo=%-5v %s · último acceso %s\n", u.Login, u.Rol, u.Activo, u.Nombre, u.Ultimo)
		}

	case "servidor":
		b := abrir()
		defer b.Close()
		falla(servir(b, *escucha, *cert, *key, *cookieSegura, *proxy))

	case "generar-padron":
		if *salida == "" {
			*salida = "padron_sintetico.csv"
		}
		falla(generarPadron(*salida, *n, *anio, *semilla))
		fmt.Printf("Padrón sintético de %d mujeres (25-65 a 01/01/%d): %s\n", *n, *anio, *salida)

	case "generar-cohorte":
		if *salida == "" {
			*salida = fmt.Sprintf("cohorte_%d.csv", *anio)
		}
		falla(generarCohorte(*salida, *n, *anio, *semilla))
		fmt.Printf("Cohorte sintética de %d mujeres que cumplen 25 en %d: %s\n", *n, *anio, *salida)

	case "cargar-padron":
		b := abrir()
		defer b.Close()
		f := hoy()
		if *fecha != "" {
			f = fechaObl(*fecha, "fecha")
		}
		rs, err := prog(b).cargarPadron(*csvIn, f, "PADRON:"+filepath.Base(*csvIn))
		falla(err)
		audCLI(b, "IMPORTACION_PADRON", *csvIn, fmt.Sprintf("%+v", rs))
		fmt.Printf("Nuevas %d (activas %d, pendientes de cumplir 25 %d, fuera de edad %d) · actualizadas %d · errores %d\n",
			rs.Nuevas, rs.Activas, rs.Pendientes, rs.FueraEdad, rs.Actualizadas, rs.Errores)

	case "alta-anual":
		b := abrir()
		defer b.Close()
		rs, err := prog(b).altaAnual(*anio, "ALTA_ANUAL")
		falla(err)
		audCLI(b, "ALTA_ANUAL", fmt.Sprint(*anio), fmt.Sprintf("%+v", rs))
		fmt.Printf("Alta anual %d: entran %d mujeres (25 años) · salen %d (han cumplido 66)\n", *anio, rs.Entran, rs.Salen)

	case "planificar":
		b := abrir()
		defer b.Close()
		d, h := fechaObl(*desde, "desde"), fechaObl(*hasta, "hasta")
		if *lote == "" {
			*lote = "L" + fs(d) + "_" + fs(h)
		}
		rs, err := prog(b).planificar(d, h, *lote, "PLANIFICADOR")
		falla(err)
		tot := 0
		ks := make([]string, 0, len(rs.Invitaciones))
		for k, v := range rs.Invitaciones {
			tot += v
			ks = append(ks, k)
		}
		sort.Strings(ks)
		audCLI(b, "LOTE_PLANIFICADO", *lote, fmt.Sprintf("%s..%s %d invitaciones", fs(d), fs(h), tot))
		fmt.Printf("Lote %s: %d invitaciones (reglas v%d)\n", *lote, tot, b.verReglas())
		for _, k := range ks {
			fmt.Printf("  %-28s %d\n", k, rs.Invitaciones[k])
		}
		fmt.Printf("Cerradas sin respuesta: %d · aplazadas por exclusión temporal: %d · salen por edad: %d\n",
			rs.CerradasSinRespuesta, rs.AplazadasExclusion, rs.SalenPorEdad)

	case "exportar-lote":
		b := abrir()
		defer b.Close()
		if *salida == "" {
			*salida = "lote_" + *lote + ".csv"
		}
		k, err := prog(b).exportarLote(*lote, *salida)
		falla(err)
		fmt.Printf("%d invitaciones exportadas a %s (registrado como SALIDA)\n", k, *salida)

	case "cerrar-sin-respuesta":
		b := abrir()
		defer b.Close()
		f := fechaObl(*fecha, "fecha")
		tx, err := b.Begin()
		falla(err)
		k, err := prog(b).cerrarSinRespuesta(tx, f, "MANUAL")
		falla(err)
		falla(tx.Commit())
		audCLI(b, "CIERRE_SIN_RESPUESTA", fs(f), fmt.Sprint(k))
		fmt.Printf("%d invitaciones cerradas por falta de respuesta\n", k)

	case "importar-pruebas", "importar-colposcopias", "importar-no-validas", "importar-exclusiones", "importar-bajas", "importar-vacunacion", "importar-zbs":
		b := abrir()
		defer b.Close()
		if *csvIn == "" {
			falla(fmt.Errorf("falta -csv"))
		}
		pg := prog(b)
		var rs resumenImport
		var err error
		switch orden {
		case "importar-pruebas":
			rs, err = pg.importarPruebas(*csvIn)
		case "importar-colposcopias":
			rs, err = pg.importarColposcopias(*csvIn)
		case "importar-no-validas":
			rs, err = pg.importarNoValidas(*csvIn)
		case "importar-exclusiones":
			rs, err = pg.importarExclusiones(*csvIn)
		case "importar-bajas":
			rs, err = pg.importarBajas(*csvIn)
		case "importar-vacunacion":
			rs, err = pg.importarVacunacion(*csvIn)
		case "importar-zbs":
			rs, err = pg.importarZonas(*csvIn)
		}
		falla(err)
		avisoRechazos(rs)

	case "zonas":
		b := abrir()
		defer b.Close()
		zs, faltan, sinZona, err := b.listarZonas()
		falla(err)
		if *salida != "" {
			falla(escribirCSV(*salida, cabZonas, filasZonas(zs)))
			fmt.Printf("%d zonas exportadas a %s\n", len(zs), *salida)
			return
		}
		fmt.Printf("%-12s %-32s %-14s %6s %8s %8s %s\n", "CÓDIGO", "NOMBRE", "ÁREA", "CUPO", "MUJERES", "ACTIVAS", "ESTADO")
		for _, z := range zs {
			cupo, est := "-", "activa"
			if z.Cupo.Valid {
				cupo = fmt.Sprint(z.Cupo.Int64)
			}
			if !z.Activa {
				est = "inactiva"
			}
			fmt.Printf("%-12s %-32s %-14s %6s %8d %8d %s\n", z.Codigo, z.Nombre, z.Area, cupo, z.Mujeres, z.Activas, est)
		}
		for _, z := range faltan {
			fmt.Printf("%-12s %-32s %-14s %6s %8d %8d %s\n", z.Codigo, "(sin catalogar)", "", "-", z.Mujeres, z.Activas, "falta en el catálogo")
		}
		if sinZona > 0 {
			fmt.Printf("%d mujeres sin zona en el padrón\n", sinZona)
		}

	case "fusionar":
		b := abrir()
		defer b.Close()
		det, err := prog(b).fusionar(*cip, *dup, *motivo)
		falla(err)
		fmt.Printf("Fusionado %s en %s: %s\n", *dup, *cip, det)

	case "reincorporar":
		b := abrir()
		defer b.Close()
		falla(prog(b).reincorporar(*cip, fechaObl(*fecha, "fecha"), *motivo))
		audCLI(b, "REINCORPORACION", *cip, *motivo)
		fmt.Println("Reincorporada.")

	case "pendientes":
		b := abrir()
		defer b.Close()
		f := hoy()
		if *fecha != "" {
			f = fechaObl(*fecha, "fecha")
		}
		ts, err := b.pendientes(strings.ToUpper(*servicio), f)
		falla(err)
		audCLI(b, "PENDIENTES_CONSULTA", *servicio, fmt.Sprintf("%d tareas", len(ts)))
		venc := 0
		for _, t := range ts {
			if t.Vencida {
				venc++
			}
		}
		fmt.Printf("%d tareas pendientes a %s (%d fuera de plazo)\n", len(ts), fs(f), venc)
		fmt.Printf("%-12s %-62s %-14s %-10s %5s %5s %s\n", "SERVICIO", "TAREA", "CIP", "DESDE", "DÍAS", "PLAZO", "DETALLE")
		for _, t := range ts {
			marca := " "
			if t.Vencida {
				marca = "!"
			}
			fmt.Printf("%-12s %-62s %-14s %-10s %4d%s %5d %s\n", t.Servicio, t.Tarea, t.CIP, fs(t.Desde), t.Dias, marca, t.Plazo, t.Detalle)
		}

	case "kpis":
		b := abrir()
		defer b.Close()
		inf, err := b.calcularKPIs(*anio)
		falla(err)
		audCLI(b, "KPIS_CONSULTA", fmt.Sprint(*anio), "")
		if *md != "" {
			f, err := os.Create(*md)
			falla(err)
			inf.markdown(f)
			f.Close()
			fmt.Printf("Informe: %s\n", *md)
		} else {
			inf.markdown(os.Stdout)
		}
		if *csvOut != "" {
			falla(inf.csv(*csvOut))
			fmt.Printf("CSV: %s\n", *csvOut)
		}

	case "trazar":
		b := abrir()
		defer b.Close()
		if *id == "" {
			*id = *cip
		}
		mid, err := b.buscarMujer(*id)
		falla(err)
		f, err := b.cargarFicha(mid)
		falla(err)
		audCLI(b, "ACCESO_FICHA", f.CIP, fmt.Sprintf("id %d", mid))
		f.markdown(os.Stdout)

	case "estado":
		b := abrir()
		defer b.Close()
		falla(b.estado(os.Stdout))

	case "simular":
		if !esPostgres(*rutaBD) {
			if _, err := os.Stat(*rutaBD); err == nil {
				falla(fmt.Errorf("%s ya existe: la simulación crea una BD nueva (use otro -bd o bórrelo)", *rutaBD))
			}
		}
		a0, a1 := 2024, 2026
		if *desde != "" {
			fmt.Sscan(*desde, &a0)
		}
		if *hasta != "" {
			fmt.Sscan(*hasta, &a1)
		}
		b := abrir()
		defer b.Close()
		var nm int
		b.QueryRow(`SELECT COUNT(*) FROM mujeres`).Scan(&nm)
		if nm > 0 {
			falla(fmt.Errorf("la base de datos ya tiene mujeres: la simulación necesita una vacía"))
		}
		fijar := func(k, v string) {
			if b.getPar(k) != v {
				falla(b.fijarParametro(k, v, "simulador", "CLI", "configuración de la simulación"))
			}
		}
		fijar("fecha_inicio_programa", fmt.Sprintf("%d-01-01", a0))
		if *autotoma != "" {
			fijar("autotoma_inicio", fs(fechaObl(*autotoma, "autotoma")))
		}
		if *estrategia != "" {
			fijar("autotoma_estrategia", strings.ToUpper(*estrategia))
		}
		dir := "."
		if !esPostgres(*rutaBD) {
			dir = filepath.Dir(*rutaBD)
		}
		ruta := filepath.Join(dir, fmt.Sprintf("padron_sintetico_%d.csv", *n))
		t0 := time.Now()
		falla(generarPadron(ruta, *n, a0, *semilla))
		pg := Programa{BD: b, Usuario: "simulador", Canal: "SIMULADOR"}
		rs, err := pg.cargarPadron(ruta, time.Date(a0, 1, 1, 0, 0, 0, 0, time.UTC), "SIMULADOR")
		falla(err)
		fmt.Printf("Padrón sintético: %d mujeres cargadas (%s) en %s\n", rs.Nuevas, ruta, motor(b))
		cohorte := *n * 23 / 1000 // ~2,3 % de la población diana cumple 25 cada año
		for a := a0; a <= a1; a++ {
			tc := cohorte
			if a == a0 {
				tc = 0 // el padrón inicial ya incluye a las de 25
			}
			falla(simularAnio(b, a, *semilla, tc, dir))
		}
		fmt.Printf("Simulación terminada en %s.\n", time.Since(t0).Round(time.Millisecond))

	default:
		fmt.Fprintf(os.Stderr, "Orden desconocida: %s\n\n%s", orden, ayuda)
		os.Exit(2)
	}
}

func envODefecto(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func motor(b *BD) string {
	if b.pg {
		return "PostgreSQL"
	}
	return "SQLite"
}

// leerClave toma la contraseña de PDPCX_CLAVE o la pide por consola.
func leerClave() string {
	if c := os.Getenv("PDPCX_CLAVE"); c != "" {
		return c
	}
	fmt.Fprint(os.Stderr, "Contraseña (mín. 12, letras y números): ")
	sc := bufio.NewScanner(os.Stdin)
	sc.Scan()
	return strings.TrimSpace(sc.Text())
}
