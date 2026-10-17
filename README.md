# pdpcx — Programa de Detección Precoz de Cáncer de Cérvix

Programa en Go para gestionar el cribado poblacional de cérvix de una comunidad
autónoma: registro maestro de mujeres, invitaciones por rondas, resultados,
triaje, colposcopias, lista viva de pendientes por servicio, reglas clínicas
versionadas, auditoría, traza completa y los KPIs de la plantilla de la Red.

- **Citología** de 25 a 29 años (cada 3 años) · **VPH-AR** de 30 a 65, 65 incluido (cada 5 años).
- **Autotoma** preparada: se activa con un parámetro y una fecha.
- **Servicio web con usuarios y roles** (Admisión, AP, Laboratorio, Ginecología, Coordinación…).
- **Base de datos PostgreSQL** (recomendada en producción) o **SQLite** (un fichero, para pruebas o pilotos).
- Pensado para unas **100.000 mujeres**; las pruebas sintéticas con 10.000 y
  100.000 no garantizan esa capacidad en producción.

Un único ejecutable, sin dependencias externas: no hace falta instalar gcc, ni
librerías, ni un servidor web aparte. Las dependencias de Go van en `vendor/`.

La base pública contiene solo código fuente y dependencias para compilar:
excluye ejecutables, bases de datos, datos de ejecución, exportaciones, registros,
capturas y muestras CSV con estructura de datos de pacientes, incluso las de
procedencia no verificada. Los CSV `plantillas/capacidad_centros.csv` y
`plantillas/capacidad_zbs.csv` son esquemas independientes de capacidad; no
constituyen un formato admitido por los importadores actuales.

---

## 1. Base de datos

El mismo programa funciona con los dos motores; se elige con `-bd` (o la
variable de entorno `PDPCX_BD`):

```bash
# PostgreSQL (producción)
export PDPCX_BD="postgres://pdpcx:CLAVE@servidor-bd:5432/pdpcx?sslmode=require"
# SQLite (pruebas, piloto, portátil)
export PDPCX_BD=cervix.db
```

SQLite usa `go-sqlite3 v0.35.6`, que traduce WASM a Go mediante wasm2go,
también en Windows. Ya no usa wazero, selector de intérprete, `RuntimeConfig`
ni importación `/embed`. La memoria queda bajo la gestión del nuevo driver:
no se mantienen los límites de páginas del intérprete anterior (256 MiB en
64 bits; 32 MiB en 32 bits). Se conservan el pool de una conexión y el journal
DELETE; PostgreSQL no cambia.

La actualización mejora el rendimiento en el diagnóstico sintético de 10.000 y
100.000 mujeres, sin cambiar consultas, esquema ni reglas clínicas. No demuestra
capacidad para 100.000 en producción ni una corrección del fallo exacto de Inicio,
cuya causa no se ha identificado. Faltan medidas de memoria/RSS, ejecución
prolongada, historias reales y más usuarios concurrentes. Pendientes puede
producir unos 4 MB de HTML con 100.000; la paginación sigue pendiente.

Para PostgreSQL basta con una base vacía y un usuario propietario:

```sql
CREATE ROLE pdpcx LOGIN PASSWORD '...';
CREATE DATABASE pdpcx OWNER pdpcx ENCODING 'UTF8';
```

`pdpcx init` crea las tablas, los parámetros por defecto y la versión 1 de las
reglas. Es idempotente.

> Las bases creadas con la versión anterior (solo SQLite, sin usuarios) no se
> migran: el esquema ha cambiado. Hay que crear una nueva y volver a cargar el
> padrón y los resultados.

## 2. Compilar

```bash
go build -mod=vendor -o pdpcx .                                   # Linux
GOOS=windows GOARCH=amd64 go build -mod=vendor -o pdpcx.exe .     # Windows
```

Go 1.26.0 o superior, instalado previamente. Los ejecutables no se incluyen en
el repositorio público: compile desde el código fuente con las órdenes anteriores.
Se conserva `vendor/`, incluido el backend wasm2go y sus licencias, para compilar
sin caché de módulos ni red. Para impedir descargas de módulos o del toolchain,
use `GOPROXY=off GOTOOLCHAIN=local` junto con `-mod=vendor`.

## 3. Servicio web

```bash
pdpcx init
# Primer administrador (la clave se pide por teclado o se lee de PDPCX_CLAVE)
pdpcx usuario-crear -login admin -nombre "Administración PDPCX" -rol ADMIN
pdpcx servidor -escucha 127.0.0.1:8080
```

El servidor **no arranca si no hay ningún ADMIN activo**. Cada usuario nuevo
debe cambiar la clave en su primer acceso.

### Despliegue

| Situación | Cómo arrancarlo |
|---|---|
| Detrás de un proxy inverso (nginx, Apache, balanceador) que pone el TLS | `servidor -escucha 127.0.0.1:8080 -cookie-segura -proxy` (`-proxy` hace que la IP de auditoría se tome de `X-Forwarded-For`) |
| TLS directo | `servidor -escucha 0.0.0.0:8443 -cert cert.pem -key clave.pem` (activa HSTS y cookie segura) |
| Pruebas en un puesto | `servidor -escucha 127.0.0.1:8080` |

No se debe publicar en una red sin TLS: las claves viajarían en claro.

### Roles

| Acción | ADMIN | COORDINACION | ADMISION | AP | LABORATORIO | GINECOLOGIA | CONSULTA |
|---|:-:|:-:|:-:|:-:|:-:|:-:|:-:|
| Buscar y ver la ficha de una mujer | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ | |
| Registrar prueba (toma / resultado) | ✔ | ✔ | | ✔ | ✔ | | |
| Registrar colposcopia / histología | ✔ | ✔ | | | | ✔ | |
| Exclusiones, cartas devueltas | ✔ | ✔ | ✔ | | | | | |
| Reincorporar, fusionar duplicados | ✔ | ✔ | | | | | | |
| Lista viva de pendientes | ✔ | ✔ (todas) | su servicio | su servicio | su servicio | su servicio | |
| KPIs (agregados, sin datos personales) | ✔ | ✔ | | | | | ✔ |
| Planificar y exportar lotes | ✔ | ✔ | | | | | |
| Importar ficheros | ✔ | ✔ | | | solo pruebas | | |
| Catálogo de zonas básicas de salud | ✔ | ✔ | | | | | |
| Cambiar reglas (parámetros) | ✔ | ✔ | | | | | |
| Usuarios, auditoría | ✔ | | | | | | |

### Seguridad incluida

- Claves con PBKDF2-SHA256 (600.000 iteraciones, sal por usuario); mínimo 12
  caracteres con letras y números.
- Bloqueo de 15 minutos tras `max_intentos_login` fallos (5); cambio de clave
  obligatorio en el primer acceso y tras un restablecimiento.
- Sesiones en la base de datos (solo se guarda el hash del testigo), caducidad
  por inactividad (`sesion_minutos`, 60), cierre de todas al cambiar la clave.
- Cookie `HttpOnly` + `SameSite=Strict` (+ `Secure` con TLS), testigo CSRF en
  cada formulario, cabeceras CSP, `X-Frame-Options: DENY`, `no-store`, HSTS con TLS.
- Sin JavaScript en las páginas.
- **Auditoría** de cada acceso a una ficha, alta, cambio, importación,
  exportación, cambio de regla, inicio de sesión (correcto, fallido, bloqueado) y
  acceso denegado, con usuario, rol, canal (WEB/CLI), IP y detalle.

### Planificación mensual

ADMIN y COORDINACION disponen de la pestaña **Planificación mensual**
(`/planificacion`), con selector de año y doce meses. Muestra citología, VPH,
controles, reinvitaciones y salidas por edad. Los controles y las reinvitaciones
se cuentan aparte del cribado ordinario, sin duplicar los totales.

- Un mes generado muestra las cifras reales y la descarga del CSV.
- El siguiente mes pendiente muestra la previsión y **Generar lote**.
- Los meses posteriores muestran solo la previsión. Se permiten meses futuros,
  sin tope mensual, pero deben generarse en orden, incluso entre años.
- La primera planificación mensual empieza en enero del año elegido. Los meses
  vacíos también quedan registrados y no pueden repetirse.
- Los lotes se llaman `AAAA-MM`; esos nombres están reservados para esta pestaña.
  **Invitaciones** conserva el histórico y los periodos libres con otros nombres.

La previsión y la generación comparten el cálculo de edad, prueba, autotoma,
exclusiones y cierres por falta de respuesta. La tarjeta de citas vencidas
muestra las mujeres activas con cita anterior a hoy y sin invitación abierta.
El lote incorpora las citas anteriores al comienzo del mes que sigan siendo
elegibles. El cálculo de la previsión no escribe en la base de datos.

La previsión usa la agenda actual y supone que no llegan nuevos resultados ni
cambian los datos. Incluye los cierres automáticos conocidos al inicio de cada
mes, pero no inventa futuros resultados, controles o repeticiones. Por eso los
meses lejanos pueden aumentar al actualizar la agenda. Si se cambia
`edad_cito_max` o `edad_vph_min`, sus rangos deben seguir siendo contiguos y no
solaparse; una configuración incoherente impide generar un lote.

Compile la versión actual para Windows según el apartado 2; incluye también
las correcciones P0. Detenga el servidor anterior y use la misma base:

```powershell
.\pdpcx.exe servidor -bd prueba10k.db -escucha 127.0.0.1:8080
```

Al abrir una base de esta versión se añaden las tablas de planificación mensual
si faltan. No hace falta volver a crear el padrón de 10.000 personas.

### Fuera del alcance del programa (infraestructura)

- Inicio de sesión corporativo (LDAP / Directorio Activo / SSO): hoy los
  usuarios son locales.
- Cifrado en reposo: se resuelve con el cifrado del disco o del servidor
  PostgreSQL. Copias de seguridad (`pg_dump`, o copia del fichero SQLite).
- Interfaces reales con TIS/Civitas, vacunación, Selene, LIS y HCE (HL7/FHIR):
  hoy se intercambian ficheros CSV, cada uno registrado en `cargas`.
- Análisis de riesgos, registro de actividades de tratamiento y demás
  obligaciones ENS/RGPD de la organización.

## 4. Lo que hace el programa (los 8 componentes)

| Componente | Dónde |
|---|---|
| **Registro maestro y elegibilidad** | `cargar-padron`, `alta-anual`, bajas, exclusiones. Cada cambio de NHC, fecha de nacimiento, ZBS, municipio o CP queda en `historial_persona` (valor anterior y nuevo). Vacunación VPH (`importar-vacunacion`). Fusión de duplicados (`fusionar`): mueve toda la historia a la ficha principal y marca la duplicada como `FUSIONADA`. |
| **Motor de episodios / rondas** | Cada invitación es un episodio (ronda inicial, sucesivas, reinvitación, control, repetición) con su estado. |
| **Aplicación operativa** | Servicio web por rol (apartado 3). |
| **Integraciones** | Ficheros CSV de entrada y salida, con rechazos por fila; todo intercambio queda en `cargas` (ENTRADA / SALIDA / PROCESO) y en auditoría. |
| **Reglas clínicas versionadas** | Cada cambio de parámetro exige un motivo y crea una **versión de reglas** (copia completa). Cada invitación guarda la versión con la que se creó y cada decisión clínica (tabla `decisiones`) guarda versión de reglas, versión del protocolo y usuario. `pdpcx versiones` o la página *Reglas*. |
| **Lista viva** | `pendientes` o la página *Pendientes*: tareas y sus plazos por servicio. |
| **Analítica / KPIs** | `kpis` o la página *KPIs* (con descarga CSV). |
| **Auditoría y seguridad** | Tabla `auditoria`, página *Auditoría*, apartado 3. |

**Principio de integración.** El programa no escribe en ninguna base de un
sistema clínico externo: recibe y entrega ficheros (o, en el futuro, mensajes de
una interfaz) y registra cada intercambio con fecha, usuario, fichero, filas
leídas, aplicadas y rechazadas.

### Lista viva: tareas y plazos

| Servicio | Tarea | Plazo (parámetro) |
|---|---|---|
| ADMISION | Invitación sin respuesta → recordatorio | `plazo_recordatorio_dias` (60) |
| ADMISION | Carta devuelta → revisar dirección | — |
| AP | Triaje que necesita toma por profesional (autotoma VPH+); repetición de muestra no válida | `plazo_ap_dias` (30) |
| LABORATORIO | Triaje réflex pendiente | `plazo_laboratorio_dias` (15) |
| GINECOLOGIA | Derivada sin colposcopia | `plazo_colposcopia_dias` (60) |
| GINECOLOGIA | Colposcopia con biopsia sin histología | `plazo_histologia_dias` (15) |
| COORDINACION | Seguimiento clínico a valorar para reincorporación | `meses_valorar_reincorporacion` (24) |

## 5. Ciclo anual de trabajo (línea de órdenes)

Todo lo que hace la web se puede hacer también por línea de órdenes (útil para
procesos programados). Las acciones quedan auditadas con el usuario `-usuario`
(o `PDPCX_USUARIO`) y canal `CLI`.

| Paso | Orden | Qué hace |
|---|---|---|
| 1 | `cargar-padron -csv padron.csv -fecha 2027-01-01` | Alta o actualización de mujeres (idempotente, con historial de cambios). |
| 2 | `alta-anual -anio 2027` | **Entran** las que cumplen 25 en el año (se citan en su cumpleaños). **Salen** las que ya han cumplido 66. |
| 3 | `importar-bajas`, `importar-exclusiones`, `importar-vacunacion` | Fallecimientos, traslados, histerectomías, embarazos, vacunas. |
| 4 | `planificar -desde 2027-01-01 -hasta 2027-01-31 -lote ENE27` | Genera las invitaciones que tocan. |
| 5 | `exportar-lote -lote ENE27 -salida ene27.csv` | Fichero para imprenta / SMS (registrado como SALIDA). |
| 6 | `importar-no-validas`, `importar-pruebas`, `importar-colposcopias` | Cada resultado hace avanzar el episodio según el protocolo. |
| 7 | `pendientes -servicio GINECOLOGIA` | Lista viva. |
| 8 | `kpis -anio 2027 -md kpis2027.md -csv-salida kpis2027.csv` | Celdas de la plantilla de la Red + indicadores + comprobaciones. |
| — | `trazar -id <CIP o NHC>` · `estado` · `versiones` | Historia de una mujer, resumen, versiones de reglas. |

Los intercambios CSV usan separador `;`, UTF-8 y cabecera. Las muestras con
estructura de pacientes no se incluyen en esta base pública; los dos esquemas
de capacidad conservados en `plantillas/` no son plantillas de importación.
Una fila errónea no detiene la carga: se aplican las demás y los rechazos se
escriben en `<fichero>.rechazos.csv` con el motivo.

### Identificación de resultados y fusión de duplicados

Las importaciones de pruebas y colposcopias requieren `cip` o
`invitacion_id`. Si se aportan los dos, deben identificar a la misma mujer;
una discrepancia rechaza la fila sin cambiar ninguna ficha. El ID de invitación
debe existir y ser un entero positivo. Los CIP de registros fusionados siguen
identificando a la ficha principal, incluso después de varias fusiones.

La fusión conserva el estado que impide el cribado rutinario, por ejemplo
`SEGUIMIENTO_CLINICO`, y elimina la cita de cribado de esa ficha. Si ambas fichas
son `ACTIVA`, un control pendiente conserva su episodio de origen y tiene
preferencia sobre una cita rutinaria; las exclusiones temporales aplazan la
próxima cita y se comprueba el límite de edad. El historial de ambas fichas se
traslada a la principal.

La operación se rechaza sin cambios cuando hay fechas de nacimiento distintas,
varios episodios abiertos, estados restrictivos incompatibles, controles de
episodios distintos, un episodio abierto incompatible con el seguimiento o una
exclusión definitiva sin resolver en una ficha activa. Estas situaciones deben
revisarse antes de repetir la fusión. Las vacunas de igual fecha con distinta
dosis o vacuna también requieren revisión; las idénticas se consolidan dejando
las dos procedencias en la traza.

### Zonas básicas de salud

El programa no fija cuántas zonas hay: cada comunidad carga su catálogo en la
página *Zonas* (a mano o subiendo un CSV) o con `importar-zbs -csv zbs.csv`.
`pdpcx zonas` lo lista y `pdpcx zonas -salida zbs.csv` lo exporta.

| Columna | Contenido |
|---|---|
| `codigo` | Obligatoria. El mismo código que llega en la columna `zbs` del padrón (se igualan mayúsculas y espacios). |
| `nombre` | Nombre de la zona (si falta, se usa el código). |
| `area` | Área o sector de salud al que pertenece. |
| `centro` | Centro de salud de referencia, donde se hacen las tomas. |
| `municipio` | Municipio cabecera. |
| `cupo_mensual` | Invitaciones al mes que la zona puede asumir. Vacío = sin tope. |
| `activa` | `S` / `N` (vacío = `S`). Una zona fusionada o cerrada se desactiva, no se borra. |
| `notas` | Texto libre (días de consulta de la matrona, observaciones…). |

Importar es acumulativo: las zonas que ya existen se actualizan y las que no
vienen en el fichero no se tocan. La página avisa de las zonas que aparecen en
el padrón y faltan en el catálogo (y permite añadirlas de una vez) y de las
mujeres sin zona. El cupo mensual todavía no lo aplica el planificador: queda
guardado para la planificación mensual por zona.

### Reglas de edad

- Se invita según la **edad en la fecha de invitación**: 25-29 citología, 30-65 VPH.
- **Nunca se invita a una mujer que ya ha cumplido 66.** Si su próxima cita cae
  después, pasa a `SALIDA_EDAD` sin invitación.
- Al cargar el padrón por primera vez, la primera ronda se reparte a lo largo
  del intervalo desde `fecha_inicio_programa`; las cercanas a 66 se adelantan.

## 6. Protocolo implementado

| Prueba primaria | Resultado | Siguiente paso |
|---|---|---|
| Citología | Negativa | Rutina a 3 años |
| Citología | ASC-US / LSIL | VPH de triaje: + → colposcopia; − → control a 12 meses |
| Citología | ASC-H / HSIL / AGC / AIS / carcinoma | Colposcopia |
| VPH-AR | Negativo | Rutina a 5 años |
| VPH-AR | 16/18 | Colposcopia |
| VPH-AR | Otros AR | Citología de triaje: anormal → colposcopia; normal → control a 12 meses |
| Control 12 m | VPH + o citología ≥ ASC-US | Colposcopia |
| Control 12 m | Negativo | Rutina |
| Cualquiera | Muestra no válida | Repetir (máx. `max_repeticiones`) |
| Colposcopia | CIN2+ / AIS / invasivo | Sale a **seguimiento clínico** (`reincorporar` la devuelve al cribado) |
| Colposcopia | Sin lesión / LSIL / sin biopsia | Vuelve al cribado a los `meses_tras_alta_colposcopia` |
| Colposcopia | No acude | Se cierra; reinvitación a los `meses_tras_no_responde` |

Con **autotoma** y VPH de otros AR, el triaje necesita una toma por profesional:
el episodio queda en `TRIAJE_PENDIENTE` y la traza lo indica.

Todos los intervalos y edades son **parámetros** (`pdpcx parametro`). Los que
dependen de una decisión clínica del comité están marcados en su descripción.

## 7. Autotoma

```bash
pdpcx parametro autotoma_inicio 2027-03-01 -motivo "Acuerdo comité 12/2026"
pdpcx parametro autotoma_estrategia NO_RESPONDEDORAS -motivo "..."   # o TODAS
```

- `NO_RESPONDEDORAS`: el kit se ofrece en la reinvitación de quien no respondió.
- `TODAS`: toda invitación de VPH se hace con autotoma.
- La invitación guarda la muestra ofrecida (`PROF`/`AUTO`) y cada prueba la
  muestra real, así que en los KPIs salen por separado (columnas C y D).

## 8. KPIs (plantilla de la Red)

- **Participación**: mujeres con invitación de cribado fechada en el año (una por
  mujer: válida > con resultado > la primera).
- **Resultados**: mujeres con muestra primaria válida tomada en el año. Si una
  mujer tiene varios episodios, cuenta una vez en su **categoría más grave**,
  para que cuadren las sumas de la plantilla.
- Los **controles a 12 meses** son invitaciones de tipo `CONTROL` y no cuentan
  en esas celdas.
- C4/D4 (población INE) se fijan con los parámetros `ine_poblacion_cito` e
  `ine_poblacion_vph`.
- El informe incluye indicadores derivados (participación, positividad,
  derivación, cumplimiento y tiempos de colposcopia, tasa de detección CIN2+) y
  una tabla de **comprobaciones** que debe salir toda en OK.

Como los resultados se guardan con **códigos cerrados** (NEGATIVA, ASCUS,
POS_1618…), no hay clasificación por texto: lo que se carga es lo que se cuenta.

## 9. Simulación (datos sintéticos)

```bash
pdpcx simular -bd prueba.db -n 10000 -desde 2024 -hasta 2026 -autotoma 2026-01-01
pdpcx kpis -bd prueba.db -anio 2025
```

Crea un padrón **sintético** (no son personas reales) y simula invitaciones,
respuestas, resultados, triaje y colposcopias con las mismas funciones que la
carga real. Las probabilidades son ilustrativas. Solo funciona sobre una base
vacía. En la simulación la lista viva sale muy cargada porque nadie corrige las
direcciones ni cierra las tareas.

### Rendimiento histórico (Linux, un núcleo; anterior a la actualización del driver)

Estas cifras no caracterizan el runtime actual ni garantizan capacidad de producción.

| | SQLite | PostgreSQL 16 |
|---|---:|---:|
| Simular 100.000 mujeres × 3 años | 1 min 50 s | 4 min 09 s |
| KPIs de un año (100.000) | 2,4 s | 0,5 s |
| Lista viva completa (100.000) | — | 0,2 s |
| Ficha / traza de una mujer | < 0,1 s | 0,04 s |
| Planificar un mes (100.000) | — | 5,7 s |

SQLite admite un solo escritor a la vez: vale para un piloto o pocos usuarios.
Con varios servicios trabajando a la vez, PostgreSQL.

## 10. Tests

```bash
go test -mod=vendor ./...                                   # SQLite
PDPCX_TEST_PG="postgres://usuario:clave@localhost/pdpcx_test" go test -mod=vendor ./...   # también PostgreSQL
```

- Tabla de decisiones del protocolo (18 casos) y límites de edad (65/66, 25, 30).
- Versionado de reglas: un cambio de parámetro crea versión y las decisiones
  posteriores la usan.
- Registro maestro: historial de cambios, vacunación, fusión de duplicados.
- Regresiones P0: identidad en importaciones de pruebas y colposcopias, CIP
  fusionados, rollback web sin bloqueo ni escrituras parciales, conservación del
  seguimiento clínico, controles, exclusiones y procedencia de vacunas.
- Planificación mensual: previsión frente a cifras reales, agenda de 10.000
  mujeres, 29→30 y 65→66, intervalos de 36/60 meses, meses vacíos, orden entre
  años, generación simultánea, rollback completo, permisos y CSRF.
- Lista viva por servicio.
- Simulación 3.000 × 3 años con 16 reglas de integridad.
- Web de extremo a extremo: redirección al login, cabeceras de seguridad, cambio
  de clave obligatorio, CSRF, permisos por rol, auditoría de accesos, registro
  de prueba (AP) y colposcopia (Ginecología), exportación registrada, cambio de
  regla, bloqueo por intentos, cierre de sesión.

## Estructura

| Fichero | Contenido |
|---|---|
| `db.go` | Esquema (SQLite y PostgreSQL), parámetros, versiones de reglas, auditoría, traza |
| `padron.go` | Carga de padrón con historial, alta anual, generador sintético |
| `maestro.go`, `fusion.go` | Vacunación, fusión de duplicados, lista viva |
| `zonas.go` | Catálogo de zonas básicas de salud |
| `fusion.go` | Conciliación del estado, citas y vacunas antes de fusionar |
| `planificador.go` | Invitaciones, reglas de edad, autotoma, lotes |
| `motor_planificacion.go` | Cálculo compartido de previsión y generación |
| `planificacion_mensual.go` | Calendario anual, orden y registro de meses |
| `web_planificacion.go` | Página y generación mensual con el permiso de lotes |
| `protocolo.go` | Decisiones clínicas y máquina de estados |
| `importar.go` | Cargas CSV |
| `kpis.go` | Celdas de la plantilla de la Red e indicadores |
| `traza.go` | Ficha, traza y estado |
| `usuarios.go` | Usuarios, claves, sesiones, auditoría |
| `web.go`, `web/` | Servicio web y plantillas |
| `simulador.go` | Simulación sintética |
| `pdpcx_test.go` | Tests |
| `p0_test.go` | Regresiones de identidad, rollback web y fusión |
| `planificacion_mensual_test.go` | Previsión, generación, concurrencia y web mensual |

## 11. Test para 10.000 personas


### 11.1
.\pdpcx.exe simular `
  -bd prueba10k.db `
  -n 10000 `
  -desde 2024 `
  -hasta 2026 `
  -autotoma 2026-01-01
### 11.2
Cree una clave propia cuando `usuario-crear` la solicite por teclado (mínimo
12 caracteres, con letras y números). En PowerShell, si `PDPCX_CLAVE` ya está
definida en la sesión, quite esa variable antes de ejecutar la orden para
usar la petición interactiva:

```powershell
Remove-Item Env:PDPCX_CLAVE -ErrorAction SilentlyContinue
.\pdpcx.exe usuario-crear `
  -bd prueba10k.db `
  -login admin `
  -nombre "Administración" `
  -rol ADMIN

.\pdpcx.exe servidor -bd prueba10k.db -escucha 127.0.0.1:8080
```
### 11.3
Usuario: admin
Clave: la que haya creado de forma interactiva; no hay una clave compartida
incluida en esta guía. Cámbiela cuando se solicite en el primer acceso.
### 11.4
.\pdpcx.exe estado -bd prueba10k.db
.\pdpcx.exe kpis -bd prueba10k.db -anio 2025
