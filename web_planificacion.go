package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

func anioPlanificacion(s string) (int, error) {
	if s == "" {
		return hoy().Year(), nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1900 || n > 9998 {
		return 0, fmt.Errorf("indique un año entre 1900 y 9998")
	}
	return n, nil
}

func (sv *Servidor) planificacionMensualWeb(w http.ResponseWriter, r *http.Request, s *Sesion) {
	anio, err := anioPlanificacion(r.URL.Query().Get("anio"))
	if err != nil {
		sv.fallo(w, s, http.StatusBadRequest, err.Error())
		return
	}
	cal, err := sv.pg(s).calendarioMensual(anio, hoy())
	if err != nil {
		sv.fallo(w, s, http.StatusInternalServerError, err.Error())
		return
	}
	sv.render(w, "planificacion", Pagina{Titulo: "Planificación mensual", S: s, D: cal, Msg: r.URL.Query().Get("msg"), Err: r.URL.Query().Get("error")})
}

func (sv *Servidor) generarMesWeb(w http.ResponseWriter, r *http.Request, s *Sesion) {
	anio, err := anioPlanificacion(r.PostFormValue("anio"))
	if err != nil {
		sv.fallo(w, s, http.StatusBadRequest, err.Error())
		return
	}
	mes, err := strconv.Atoi(r.PostFormValue("mes"))
	if err != nil || mes < 1 || mes > 12 {
		sv.fallo(w, s, http.StatusBadRequest, "Mes no válido")
		return
	}
	lote := fmt.Sprintf("%04d-%02d", anio, mes)
	rs, err := sv.pg(s).generarMes(anio, mes, "WEB")
	q := url.Values{"anio": {strconv.Itoa(anio)}}
	if err != nil {
		sv.aud(r, s, "LOTE_MENSUAL_ERROR", lote, err.Error())
		q.Set("error", err.Error())
	} else {
		total := 0
		for _, n := range rs.Invitaciones {
			total += n
		}
		msg := fmt.Sprintf("Lote %s generado: %d invitaciones. Cerradas sin respuesta: %d. Salen por edad: %d.", lote, total, rs.CerradasSinRespuesta, rs.SalenPorEdad)
		sv.aud(r, s, "LOTE_MENSUAL_GENERADO", lote, msg)
		q.Set("msg", msg)
	}
	http.Redirect(w, r, "/planificacion?"+q.Encode(), http.StatusSeeOther)
}
