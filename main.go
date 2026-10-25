package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // zona horaria America/Guayaquil embebida

	"cloud.google.com/go/firestore"
	"github.com/cloudinary/cloudinary-go/v2"
	"github.com/cloudinary/cloudinary-go/v2/api/uploader"
	"github.com/joho/godotenv"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ═════════════ main.go ═════════════
const defaultBrevoEmail = "ad85ef001@smtp-brevo.com"

var (
	db          *firestore.Client
	cld         *cloudinary.Cloudinary
	ctx         = context.Background()
	brevoKey    string
	brevoSender string
)

// 🔒 No exponer código fuente, .env, etc.
var reSensible = regexp.MustCompile(`\.(js|json|env|md|lock|map|log|go|mod|sum)$`)

func bloquearSensibles(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.ToLower(r.URL.Path)
		bad := reSensible.MatchString(p) || strings.Contains(p, "node_modules")
		for _, seg := range strings.Split(p, "/") {
			if strings.HasPrefix(seg, ".") {
				bad = true
			}
		}
		if bad {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func corsMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		if rh := r.Header.Get("Access-Control-Request-Headers"); rh != "" {
			h.Set("Access-Control-Allow-Headers", rh)
		} else {
			h.Set("Access-Control-Allow-Headers", "Content-Type, x-admin-token")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func recoverMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("❌ panic en %s %s: %v", r.Method, r.URL.Path, rec)
				fail(w, 500, "Error interno del servidor")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// 🔒 Si defines ADMIN_TOKEN, las rutas sensibles exigen el header x-admin-token.
func requiereAdmin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := os.Getenv("ADMIN_TOKEN")
		if token == "" || r.Header.Get("x-admin-token") == token {
			h(w, r)
			return
		}
		fail(w, 401, "No autorizado")
	}
}

// Evita listados de directorio en el servidor estático.
type dirSeguro struct{ fs http.FileSystem }

func (d dirSeguro) Open(name string) (http.File, error) {
	f, err := d.fs.Open(name)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st.IsDir() {
		idx, err := d.fs.Open(strings.TrimSuffix(name, "/") + "/index.html")
		if err != nil {
			f.Close()
			return nil, os.ErrNotExist
		}
		idx.Close()
	}
	return f, nil
}

func main() {
	_ = godotenv.Load()

	// 1. Firebase / Firestore
	raw := os.Getenv("FIREBASE_SERVICE_ACCOUNT")
	if raw == "" {
		log.Fatal("❌ ERROR CRÍTICO: La variable de entorno FIREBASE_SERVICE_ACCOUNT no está configurada.")
	}
	var sa struct {
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal([]byte(raw), &sa); err != nil {
		log.Fatalf("❌ Error crítico al procesar FIREBASE_SERVICE_ACCOUNT: %v", err)
	}
	var err error
	db, err = firestore.NewClient(ctx, sa.ProjectID, option.WithCredentialsJSON([]byte(raw)))
	if err != nil {
		log.Fatalf("❌ Error crítico al conectar con Firestore: %v", err)
	}
	defer db.Close()
	log.Println("✅ ¡Conectado exitosamente a Firebase Cloud Firestore!")

	// 1.b Cloudinary
	cn, ck, cs := os.Getenv("CLOUDINARY_CLOUD_NAME"), os.Getenv("CLOUDINARY_API_KEY"), os.Getenv("CLOUDINARY_API_SECRET")
	if cn == "" || ck == "" || cs == "" {
		log.Println("⚠️ Cloudinary no configurado (faltan variables CLOUDINARY_*). El módulo de Archivos no funcionará.")
	} else if cld, err = cloudinary.NewFromParams(cn, ck, cs); err != nil {
		log.Printf("⚠️ Error configurando Cloudinary: %v", err)
		cld = nil
	} else {
		log.Println("☁️ Cloudinary configurado. Listo para subir archivos.")
	}

	// 2. Brevo
	brevoKey = os.Getenv("BREVO_API_KEY")
	brevoSender = envOr("BREVO_SENDER_EMAIL", defaultBrevoEmail)
	if brevoKey != "" {
		log.Println("📧 Brevo API (HTTP) configurada. Listo para enviar correos.")
		iniciarGuardianCaducidades()
	} else {
		log.Println("⚠️ BREVO_API_KEY no configurado. Envío de correos inhabilitado.")
	}

	// Rutas
	mux := http.NewServeMux()
	registrarRutas(mux)

	publicDir := filepath.Join(".", "public")
	if st, e := os.Stat(publicDir); e == nil && st.IsDir() {
		mux.Handle("GET /", http.FileServer(dirSeguro{http.Dir(publicDir)}))
		log.Println("🔒 Sirviendo archivos estáticos solo desde /public")
	} else {
		mux.Handle("GET /", http.FileServer(dirSeguro{http.Dir(".")}))
		log.Println("⚠️ No existe la carpeta /public: se sirve la raíz del proyecto (menos seguro).")
	}

	handler := corsMW(recoverMW(bloquearSensibles(mux)))

	port := envOr("PORT", "10000")
	log.Printf("🚀 Motor NEXUS encendido en el puerto: %s", port)
	log.Fatal(http.ListenAndServe("0.0.0.0:"+port, handler))
}

func registrarRutas(mux *http.ServeMux) {
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		send(w, M{"ok": true, "message": "NEXUS Core Engine activo", "time": nowISO()})
	})
	mux.HandleFunc("GET /inventario/forzar-alerta", func(w http.ResponseWriter, r *http.Request) {
		ejecutarRevisionCaducidades()
		send(w, M{"ok": true, "mensaje": "Escaneo del guardián forzado manualmente."})
	})

	// SRI / Invoka
	mux.HandleFunc("POST /sri/configurar", requiereAdmin(sriConfigurar))
	mux.HandleFunc("POST /sri/firma", requiereAdmin(sriFirma))
	mux.HandleFunc("GET /sri/estado", requiereAdmin(sriEstado))

	// Productos
	mux.HandleFunc("GET /productos", getProductos)
	mux.HandleFunc("POST /productos", postProducto)
	mux.HandleFunc("PUT /productos/agregar/{id}", agregarStock)
	mux.HandleFunc("PUT /productos/vender/{id}", venderProducto)
	mux.HandleFunc("PUT /productos/{id}", putProducto)
	mux.HandleFunc("DELETE /productos/{id}", deleteProducto)

	// Movimientos de inventario
	mux.HandleFunc("GET /movimientos-inventario", getMovimientos)
	mux.HandleFunc("POST /movimientos-inventario", postMovimiento)
	mux.HandleFunc("GET /inventario/caducidades", getCaducidades)

	// Clientes
	mux.HandleFunc("POST /clientes", postCliente)
	mux.HandleFunc("GET /clientes", getClientes)
	mux.HandleFunc("POST /clientes/sumar-deuda", sumarDeuda)
	mux.HandleFunc("POST /clientes/abonar", abonarCliente)
	mux.HandleFunc("PUT /clientes/editar", editarCliente)
	mux.HandleFunc("GET /clientes/{cedula}", getClientePorCedula)
	mux.HandleFunc("DELETE /clientes/{id}", deleteCliente)

	// Correo
	mux.HandleFunc("POST /correo/factura", correoFactura)
	mux.HandleFunc("POST /correo/abono", correoAbono)

	// Ventas
	mux.HandleFunc("POST /ventas", postVenta)
	mux.HandleFunc("DELETE /ventas/producto/{ventaId}/{indice}", deleteProductoVenta)
	mux.HandleFunc("DELETE /ventas/dia", requiereAdmin(deleteVentasDia))

	// Deudas
	mux.HandleFunc("GET /deudas", getDeudas)
	mux.HandleFunc("POST /deudas", postDeuda)
	mux.HandleFunc("POST /deudas/pagar", pagarDeuda)
	mux.HandleFunc("PUT /deudas/{id}", putDeuda)
	mux.HandleFunc("DELETE /deudas/{id}", deleteDeuda)

	// Pedidos
	mux.HandleFunc("GET /pedidos", getPedidos)
	mux.HandleFunc("POST /pedidos", postPedido)
	mux.HandleFunc("PUT /pedidos/recibir/{id}", recibirPedido)
	mux.HandleFunc("DELETE /pedidos/{id}", deletePedido)

	// Riesgo
	mux.HandleFunc("GET /riesgo", getRiesgo)
	mux.HandleFunc("GET /riesgo/resumen", getRiesgoResumen)

	// Caja
	mux.HandleFunc("POST /caja/abrir", cajaAbrir)
	mux.HandleFunc("GET /caja", getCaja)
	mux.HandleFunc("POST /caja/gasto", cajaGasto)
	mux.HandleFunc("POST /caja/ingreso", cajaIngreso)
	mux.HandleFunc("POST /caja/transferencia", cajaTransferencia)
	mux.HandleFunc("POST /caja/cerrar", cajaCerrar)
	mux.HandleFunc("GET /caja/historial", cajaHistorial)

	// Análisis
	mux.HandleFunc("GET /analisis", getAnalisis)

	// Cheques
	mux.HandleFunc("GET /cheques", getCheques)
	mux.HandleFunc("POST /cheques", postCheque)
	mux.HandleFunc("PUT /cheques/{id}", putCheque)
	mux.HandleFunc("POST /cheques/pagar/{id}", pagarCheque)
	mux.HandleFunc("POST /cheques/despagar/{id}", despagarCheque)
	mux.HandleFunc("DELETE /cheques/{id}", deleteCheque)
	mux.HandleFunc("GET /cheques-notas", getChequesNotas)
	mux.HandleFunc("PUT /cheques-notas/{mes}", putChequesNotas)

	// Archivos
	mux.HandleFunc("GET /archivos", getArchivos)
	mux.HandleFunc("GET /archivos/ruta/{id}", getRutaArchivos)
	mux.HandleFunc("POST /archivos/carpeta", postCarpeta)
	mux.HandleFunc("POST /archivos/subir", subirArchivo)
	mux.HandleFunc("POST /archivos/enlace", postEnlace)
	mux.HandleFunc("DELETE /archivos/carpeta/{id}", deleteCarpeta)
	mux.HandleFunc("DELETE /archivos/{id}", deleteArchivo)
}

// ═════════════ util.go ═════════════
// M es el equivalente a un objeto JS: permite manejar los documentos de forma dinámica.
type M = map[string]any

var loc *time.Location

func init() {
	l, err := time.LoadLocation("America/Guayaquil")
	if err != nil {
		l = time.FixedZone("ECT", -5*3600)
	}
	loc = l
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ---------- Respuestas ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func send(w http.ResponseWriter, v any)            { writeJSON(w, 200, v) }
func fail(w http.ResponseWriter, code int, m string) { writeJSON(w, code, M{"error": m}) }
func failDet(w http.ResponseWriter, code int, m string, err error) {
	writeJSON(w, code, M{"error": m, "detalle": err.Error()})
}

// readBody lee JSON o formularios urlencoded (equivale a express.json + urlencoded).
func readBody(w http.ResponseWriter, r *http.Request) M {
	body := M{}
	if r.Body == nil {
		return body
	}
	r.Body = http.MaxBytesReader(w, r.Body, 25<<20)
	ct := r.Header.Get("Content-Type")
	switch {
	case strings.Contains(ct, "application/json"):
		_ = json.NewDecoder(r.Body).Decode(&body)
	case strings.Contains(ct, "application/x-www-form-urlencoded"):
		if r.ParseForm() == nil {
			for k, v := range r.PostForm {
				if len(v) > 0 {
					body[k] = v[0]
				}
			}
		}
	}
	if body == nil {
		body = M{}
	}
	return body
}

// ---------- Conversión de tipos (JS-like) ----------

func num(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case float32:
		return float64(t)
	case int:
		return float64(t)
	case int32:
		return float64(t)
	case int64:
		return float64(t)
	case json.Number:
		f, _ := t.Float64()
		return f
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0
		}
		return f
	case bool:
		if t {
			return 1
		}
	}
	return 0
}

func str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprint(t)
	}
}

func strOr(v any, def string) string {
	if s := str(v); s != "" {
		return s
	}
	return def
}

func nilIfEmpty(v any) any {
	if str(v) == "" {
		return nil
	}
	return v
}

func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	}
	return true
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }
func money(v any) string       { return fmt.Sprintf("%.2f", num(v)) }
func fmtNum(f float64) string  { return strconv.FormatFloat(f, 'f', -1, 64) }
func escH(s string) string     { return html.EscapeString(s) }

// amount || cantidad || 1
func cantidadDe(p M) float64 {
	if a := num(p["amount"]); a != 0 {
		return a
	}
	if c := num(p["cantidad"]); c != 0 {
		return c
	}
	return 1
}

func asList(v any) []M {
	arr, _ := v.([]any)
	out := make([]M, 0, len(arr))
	for _, e := range arr {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// ---------- Fechas ----------

func nowISO() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

func hoyISOServidor() string { return time.Now().In(loc).Format("2006-01-02") }

func parseTime(v any) (time.Time, bool) {
	switch t := v.(type) {
	case time.Time:
		return t, true
	case string:
		if t == "" {
			return time.Time{}, false
		}
		if tm, err := time.Parse(time.RFC3339, t); err == nil {
			return tm, true
		}
		if tm, err := time.ParseInLocation("2006-01-02T15:04:05", t, loc); err == nil {
			return tm, true
		}
		if tm, err := time.ParseInLocation("2006-01-02", t, loc); err == nil {
			return tm, true
		}
	}
	return time.Time{}, false
}

// Normaliza cualquier fecha (string, Timestamp de Firestore) a ISO.
func aFechaISO(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case time.Time:
		return t.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	return str(v)
}

func fechaOrNow(v any) string {
	if tm, ok := parseTime(v); ok {
		return tm.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	return nowISO()
}

func horaLocal(v any) string {
	if tm, ok := parseTime(v); ok {
		return tm.In(loc).Format("15:04:05")
	}
	return ""
}

var meses = []string{"enero", "febrero", "marzo", "abril", "mayo", "junio", "julio", "agosto", "septiembre", "octubre", "noviembre", "diciembre"}

func fechaLarga(t time.Time) string {
	t = t.In(loc)
	return fmt.Sprintf("%d de %s de %d, %02d:%02d", t.Day(), meses[t.Month()-1], t.Year(), t.Hour(), t.Minute())
}

// ---------- Firestore ----------

func mapDocs(docs []*firestore.DocumentSnapshot) []M {
	out := make([]M, 0, len(docs))
	for _, d := range docs {
		m := d.Data()
		if m == nil {
			m = M{}
		}
		m["_id"] = d.Ref.ID
		out = append(out, m)
	}
	return out
}

func getDoc(ref *firestore.DocumentRef) (*firestore.DocumentSnapshot, bool, error) {
	snap, err := ref.Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, false, nil
		}
		return nil, false, err
	}
	return snap, true, nil
}

func updateMap(ref *firestore.DocumentRef, m M) error {
	ups := make([]firestore.Update, 0, len(m))
	for k, v := range m {
		ups = append(ups, firestore.Update{Path: k, Value: v})
	}
	_, err := ref.Update(ctx, ups)
	return err
}

func registrarMovimiento(tipo, codigo, nombre string, cantidad float64, motivo string) error {
	now := time.Now().In(loc)
	_, _, err := db.Collection("movimientos-inventario").Add(ctx, M{
		"tipo":     strOr(tipo, "entrada"),
		"codigo":   strOr(codigo, "-"),
		"nombre":   strOr(nombre, "Sin Nombre"),
		"cantidad": cantidad,
		"fecha":    now.Format("2006-01-02"),
		"hora":     now.Format("15:04:05"),
		"motivo":   strOr(motivo, "Actualización manual"),
	})
	return err
}

func cajaActiva() (*firestore.DocumentSnapshot, error) {
	docs, err := db.Collection("cajas").Where("activa", "==", true).Documents(ctx).GetAll()
	if err != nil || len(docs) == 0 {
		return nil, err
	}
	return docs[0], nil
}

// Suma a la caja de forma atómica.
func cajaRegistrar(caja *firestore.DocumentSnapshot, ingresos, gastos float64, mov M) error {
	ups := []firestore.Update{}
	if ingresos != 0 {
		ups = append(ups, firestore.Update{Path: "ingresos", Value: firestore.Increment(ingresos)})
	}
	if gastos != 0 {
		ups = append(ups, firestore.Update{Path: "gastos", Value: firestore.Increment(gastos)})
	}
	if mov != nil {
		ups = append(ups, firestore.Update{Path: "movimientos", Value: firestore.ArrayUnion(mov)})
	}
	if len(ups) == 0 {
		return nil
	}
	_, err := caja.Ref.Update(ctx, ups)
	return err
}

// ═════════════ sri_correo.go ═════════════
var httpc = &http.Client{Timeout: 60 * time.Second}

func doJSON(method, url string, headers map[string]string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, nil
}

func cut(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n])
	}
	return string(b)
}

// =========================================================================
// BREVO
// =========================================================================

func enviarCorreoBrevo(to, subject, htmlBody, fromName, fromEmail string) (bool, string) {
	if brevoKey == "" {
		return false, "BREVO_API_KEY no configurado"
	}
	if fromName == "" {
		fromName = "Agro Naranjito #1"
	}
	if fromEmail == "" {
		fromEmail = brevoSender
	}
	code, raw, err := doJSON("POST", "https://api.brevo.com/v3/smtp/email",
		map[string]string{"accept": "application/json", "api-key": brevoKey},
		M{
			"sender":      M{"name": fromName, "email": fromEmail},
			"to":          []M{{"email": to}},
			"subject":     subject,
			"htmlContent": htmlBody,
		})
	if err != nil {
		log.Println("❌ Error al conectar con Brevo API:", err)
		return false, err.Error()
	}
	if code >= 200 && code < 300 {
		return true, ""
	}
	log.Println("❌ Brevo API rechazó el envío:", cut(raw, 500))
	var d M
	_ = json.Unmarshal(raw, &d)
	return false, strOr(d["message"], "Error en Brevo API")
}

// =========================================================================
// INVOKA — FACTURACIÓN ELECTRÓNICA SRI
// =========================================================================

func sriConfigurar(w http.ResponseWriter, r *http.Request) {
	key := os.Getenv("INVOKA_API_KEY")
	if key == "" {
		fail(w, 500, "INVOKA_API_KEY no configurada")
		return
	}
	_, raw, err := doJSON("POST", "https://www.invoka.com.ec/api/empresa/crear",
		map[string]string{"X-API-KEY": key},
		M{
			"ruc":               os.Getenv("INVOKA_RUC"),
			"razon_social":      os.Getenv("INVOKA_RAZON_SOCIAL"),
			"grancontribuyente": false,
			"smtp":              true,
			"smtp_host":         "smtp-relay.brevo.com",
			"smtp_port":         465,
			"smtp_user":         defaultBrevoEmail,
			"smtp_password":     os.Getenv("BREVO_SMTP_KEY"),
			"smtp_encryption":   "ssl",
			"smtp_from_email":   defaultBrevoEmail,
		})
	if err != nil {
		log.Println("❌ Error al registrar empresa en Invoka:", err)
		fail(w, 500, err.Error())
		return
	}
	log.Println("📋 Respuesta Invoka /empresa/crear:", cut(raw, 500))
	var data any
	if err := json.Unmarshal(raw, &data); err != nil {
		fail(w, 500, "Invoka devolvió una respuesta no JSON")
		return
	}
	send(w, data)
}

func sriFirma(w http.ResponseWriter, r *http.Request) {
	key := os.Getenv("INVOKA_API_KEY")
	if key == "" {
		fail(w, 500, "INVOKA_API_KEY no configurada")
		return
	}
	b := readBody(w, r)
	firma, pass := str(b["firma_base64"]), str(b["password"])
	if firma == "" || pass == "" {
		fail(w, 400, "Se requiere firma_base64 y password")
		return
	}
	code, raw, err := doJSON("POST", "https://www.invoka.com.ec/api/empresa/subirfirma",
		map[string]string{"X-API-KEY": key},
		M{"firma_base64": firma, "password": pass, "ruc": os.Getenv("INVOKA_RUC")})
	if err != nil {
		log.Println("❌ Error al subir firma a Invoka:", err)
		fail(w, 500, err.Error())
		return
	}
	log.Println("📋 Invoka /empresa/subirfirma status:", code, cut(raw, 300))

	var data M
	if err := json.Unmarshal(raw, &data); err != nil {
		writeJSON(w, 500, M{"error": fmt.Sprintf("Invoka devolvió respuesta no JSON (status %d)", code), "raw": cut(raw, 200)})
		return
	}
	if code >= 200 && code < 300 {
		log.Println("✅ Firma electrónica registrada en Invoka exitosamente")
		send(w, M{"ok": true, "mensaje": "Firma registrada correctamente en Invoka", "data": data})
		return
	}
	writeJSON(w, code, M{"ok": false, "error": strOr(data["mensaje"], strOr(data["error"], "Error en Invoka")), "data": data})
}

func sriEstado(w http.ResponseWriter, r *http.Request) {
	key := os.Getenv("INVOKA_API_KEY")
	if key == "" {
		fail(w, 500, "INVOKA_API_KEY no configurada")
		return
	}
	code, raw, err := doJSON("GET", "https://www.invoka.com.ec/api/empresa/"+os.Getenv("INVOKA_RUC"),
		map[string]string{"X-API-KEY": key}, nil)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	var data any
	if json.Unmarshal(raw, &data) != nil {
		data = M{"raw": cut(raw, 200)}
	}
	send(w, M{"status": code, "data": data})
}

// Secuencial con transacción: dos ventas simultáneas no repiten número.
func siguienteSecuencial() (string, error) {
	ref := db.Collection("config").Doc("secuencialFactura")
	var valor int64
	err := db.RunTransaction(ctx, func(_ context.Context, tx *firestore.Transaction) error {
		var cur int64
		snap, err := tx.Get(ref)
		if err == nil {
			cur = int64(num(snap.Data()["valor"]))
		} else if status.Code(err) != codes.NotFound {
			return err
		}
		valor = cur + 1
		return tx.Set(ref, M{"valor": valor})
	})
	return fmt.Sprintf("%09d", valor), err
}

type sriResult struct {
	OK           bool
	ClaveAcceso  any
	Autorizacion any
	Err          string
}

func emitirFacturaInvoka(cliente, cedula, correo string, carrito []M, descuento float64) sriResult {
	key := os.Getenv("INVOKA_API_KEY")
	if key == "" {
		log.Println("⚠️ INVOKA_API_KEY no configurada. Factura electrónica omitida.")
		return sriResult{Err: "API Key de Invoka no configurada"}
	}

	esConsumidorFinal := cedula == "" || cedula == "9999999999999" || cedula == "SIN CÉDULA"
	tipoIdent, ident := "07", "9999999999999"
	if !esConsumidorFinal {
		ident = cedula
		if len(cedula) == 13 {
			tipoIdent = "04"
		} else {
			tipoIdent = "05"
		}
	}

	fechaEmision := time.Now().In(loc).Format("2006/01/02")
	secuencial, err := siguienteSecuencial()
	if err != nil {
		return sriResult{Err: "No se pudo obtener el secuencial: " + err.Error()}
	}

	ivaPct, tipoIva := 15.0, 4.0
	if v := os.Getenv("INVOKA_IVA_PCT"); v != "" {
		ivaPct = num(v)
	}
	if v := os.Getenv("INVOKA_TIPO_IVA"); v != "" {
		tipoIva = num(v)
	}

	subtotalAcum := 0.0
	items := make([]M, 0, len(carrito))
	for i, p := range carrito {
		cantidad := cantidadDe(p)
		precio := num(p["precio"])
		subItem := precio * cantidad
		subtotalAcum += subItem
		items = append(items, M{
			"codigo_principal":          strOr(p["codigo"], fmt.Sprintf("PROD-%d", i+1)),
			"descripcion":               strOr(p["nombre"], "Producto"),
			"cantidad":                  cantidad,
			"precio_unitario":           precio,
			"descuento":                 0,
			"precio_total_sin_impuesto": round2(subItem),
			"tipoproducto":              1,
			"tipo_iva":                  tipoIva,
		})
	}

	subtotal := round2(subtotalAcum)
	totalDesc := round2(descuento)
	base := math.Max(0, subtotal-totalDesc)
	valorIva := round2(base * (ivaPct / 100))
	totalAPagar := round2(base + valorIva)

	nombreCom := envOr("INVOKA_NOMBRE_COMERCIAL", envOr("INVOKA_RAZON_SOCIAL", "AGRO NARANJITO #1"))
	dir := envOr("INVOKA_DIRECCION", "ECUADOR")

	comprador := M{
		"identificacion":      ident,
		"tipo_identificacion": tipoIdent,
		"razon_social":        strOr(cliente, "CONSUMIDOR FINAL"),
		"direccion":           "ECUADOR",
	}
	if correo != "" {
		comprador["correo"] = correo
	}

	ambiente := 1.0
	if v := os.Getenv("INVOKA_AMBIENTE"); v != "" {
		ambiente = num(v)
	}

	factura := M{
		"ambiente": ambiente,
		"emisor": M{
			"nombre_comercial":          nombreCom,
			"razon_social":              envOr("INVOKA_RAZON_SOCIAL", "AGRO NARANJITO #1"),
			"ruc":                       os.Getenv("INVOKA_RUC"),
			"codigo_establecimiento":    envOr("INVOKA_COD_ESTABLECIMIENTO", "001"),
			"codigo_puntoemision":       envOr("INVOKA_COD_PUNTOEMISION", "001"),
			"direccion_matriz":          dir,
			"direccion_establecimiento": dir,
			"obligado_contabilidad":     envOr("INVOKA_OBLIGADO_CONTABILIDAD", "NO"),
			"fecha_emision":             fechaEmision,
			"secuencial":                secuencial,
		},
		"comprador":   comprador,
		"items":       items,
		"subtotal":    subtotal,
		"discount":    totalDesc,
		"iva":         valorIva,
		"total":       totalAPagar,
		"formas_pago": []M{{"forma_pago": "20", "total": totalAPagar}},
	}

	code, raw, err := doJSON("POST", "https://www.invoka.com.ec/api/factura/emision",
		map[string]string{"X-API-KEY": key}, factura)
	if err != nil {
		log.Println("❌ Error al conectar con Invoka:", err)
		return sriResult{Err: err.Error()}
	}
	log.Printf("📋 Invoka /factura/emision status: %d — %s", code, cut(raw, 500))

	var data M
	if err := json.Unmarshal(raw, &data); err != nil {
		return sriResult{Err: fmt.Sprintf("Invoka devolvió HTML (status %d), no JSON", code)}
	}
	if truthy(data["creado"]) {
		log.Printf("✅ Factura SRI registrada — Clave: %v", data["claveacceso"])
		return sriResult{OK: true, ClaveAcceso: data["claveacceso"], Autorizacion: data["id_comprobante"]}
	}
	log.Println("⚠️ Invoka respondió sin crear factura:", cut(raw, 500))
	msg := str(data["mensaje"])
	if msg == "" {
		if b, e := json.Marshal(data["errors"]); e == nil && string(b) != "null" {
			msg = string(b)
		} else {
			msg = "Respuesta inesperada de Invoka"
		}
	}
	return sriResult{Err: msg}
}

// =========================================================================
// PLANTILLAS DE CORREO
// =========================================================================

const tplFactura = `<!DOCTYPE html>
<html lang="es">
<head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1"></head>
<body style="margin:0; padding:0; background-color:#f5f6fa; font-family:'Segoe UI',Arial,sans-serif;">
<table width="100%" cellpadding="0" cellspacing="0" style="background-color:#f5f6fa; padding:30px 0;">
<tr><td align="center">
<table width="540" cellpadding="0" cellspacing="0" style="background-color:#ffffff; border-radius:12px; overflow:hidden; box-shadow:0 4px 15px rgba(0,0,0,0.06);">
<tr><td style="background-color:#ff3f34; padding:26px 30px; text-align:center;">
<h1 style="margin:0; color:#ffffff; font-size:24px; letter-spacing:1px; font-weight:700;">AGRO NARANJITO #1</h1>
<p style="margin:4px 0 0; color:rgba(255,255,255,0.85); font-size:13px;">Comprobante Digital de Venta · {{FECHA}}</p>
</td></tr>
<tr><td style="padding:24px 30px 10px;">
<p style="margin:0 0 4px; font-size:12px; color:#888; text-transform:uppercase; letter-spacing:.5px;">Titular del Documento</p>
<p style="margin:0; font-size:17px; font-weight:600; color:#1e272e;">{{CLIENTE}}</p>
{{CEDULA}}
<span style="display:inline-block; margin-top:10px; padding:4px 14px; background-color:{{COLOR}}; color:#ffffff; border-radius:20px; font-size:11px; font-weight:600; text-transform:uppercase; letter-spacing:0.3px;">{{TIPO}}</span>
</td></tr>
<tr><td style="padding:15px 30px;">
<table width="100%" cellpadding="0" cellspacing="0" style="border-collapse:collapse; margin-top:5px;">
<thead><tr style="background-color:#f8f9fa;">
<th style="padding:10px; text-align:left; font-size:12px; color:#777; font-weight:600; text-transform:uppercase; border-bottom:1px solid #ddd;">Detalle</th>
<th style="padding:10px; text-align:center; font-size:12px; color:#777; font-weight:600; text-transform:uppercase; border-bottom:1px solid #ddd; width:50px;">Cant.</th>
<th style="padding:10px; text-align:right; font-size:12px; color:#777; font-weight:600; text-transform:uppercase; border-bottom:1px solid #ddd; width:80px;">P. Unit</th>
<th style="padding:10px; text-align:right; font-size:12px; color:#777; font-weight:600; text-transform:uppercase; border-bottom:1px solid #ddd; width:90px;">Total</th>
</tr></thead>
<tbody>{{FILAS}}</tbody>
</table>
</td></tr>
<tr><td style="padding:10px 30px 25px;">
<table width="100%" cellpadding="0" cellspacing="0">
{{DESCUENTO}}{{DETALLE}}{{SRI}}
<tr><td colspan="2"><hr style="border:none; border-top:2px dashed #f0f0f0; margin:12px 0;"></td></tr>
<tr>
<td style="font-size:16px; font-weight:700; color:#1e272e;">IMPORTE NETO RECAUDADO</td>
<td style="text-align:right; font-size:22px; font-weight:700; color:#ff3f34;">${{TOTAL}}</td>
</tr>
</table>
</td></tr>
<tr><td style="background-color:#f8f9fa; padding:20px 30px; text-align:center; border-top:1px solid #f0f0f0;">
<p style="margin:0; font-size:14px; color:#2c3e50; font-weight:500;">¡Gracias por depositar su confianza en nosotros! 😊</p>
<p style="margin:4px 0 0; font-size:11px; color:#7f8c8d;">Agro Naranjito #1 · Nota: Este documento digital es un comprobante automático de caja.</p>
</td></tr>
</table>
</td></tr>
</table>
</body>
</html>`

func filaPago(label, valor, extra string) string {
	return `<tr><td style="padding:4px 0; color:#555;">` + label + `</td><td style="text-align:right;` + extra + `">` + valor + `</td></tr>`
}

func generarHTMLCorreo(d M, carrito []M, tipoPago string) string {
	var filas strings.Builder
	for _, p := range carrito {
		cant := cantidadDe(p)
		filas.WriteString(`<tr>
<td style="padding:8px 10px; border-bottom:1px solid #f0f0f0; text-align:left;">` + escH(str(p["nombre"])) + `</td>
<td style="padding:8px 10px; border-bottom:1px solid #f0f0f0; text-align:center;">` + escH(fmtNum(cant)) + `</td>
<td style="padding:8px 10px; border-bottom:1px solid #f0f0f0; text-align:right;">$` + money(p["precio"]) + `</td>
<td style="padding:8px 10px; border-bottom:1px solid #f0f0f0; text-align:right; font-weight:bold;">$` + fmt.Sprintf("%.2f", num(p["precio"])*cant) + `</td>
</tr>`)
	}

	detalle := ""
	switch tipoPago {
	case "efectivo":
		detalle = filaPago("Monto Recibido", "$"+money(d["pago"]), " font-weight:500;") +
			filaPago("Vuelto Entregado", "$"+money(d["vuelto"]), " font-weight:500;")
	case "transferencia":
		detalle = filaPago("Entidad Bancaria", escH(str(d["bancoNombre"])), " font-weight:500;") +
			filaPago("Nº de Cuenta", escH(str(d["bancoCuenta"])), " font-family:monospace;") +
			filaPago("Nº Referencia / Código", escH(str(d["comprobante"])), " font-weight:bold; color:#1e272e;")
	case "credito":
		detalle = filaPago("Tasa Diferido ("+escH(str(d["tasaPct"]))+"%)", "+$"+money(d["montoInteres"]), " color:#e67e22;") +
			filaPago("Plazo Acordado", escH(str(d["meses"]))+" meses", " font-weight:500;")
	}

	descuento := ""
	if num(d["pct"]) > 0 {
		descuento = filaPago("Subtotal Bruto", "$"+money(d["subtotal"]), "") +
			`<tr><td style="padding:4px 0; color:#e03329;">Descuento Aplicado (` + escH(str(d["pct"])) + `%)</td><td style="text-align:right; color:#e03329; font-weight:500;">-$` + money(d["descuentoMonto"]) + `</td></tr>`
	}

	sri := ""
	if c := str(d["claveAccesoSRI"]); c != "" {
		sri = `<tr><td colspan="2" style="padding:8px 0;"><div style="background:#eafaf1; border:1px solid #27ae60; border-radius:6px; padding:8px 12px; text-align:center;">
<p style="margin:0; font-size:11px; color:#27ae60; font-weight:700;">✅ FACTURA ELECTRÓNICA AUTORIZADA — SRI</p>
<p style="margin:4px 0 0; font-size:10px; color:#555; font-family:monospace; word-break:break-all;">` + escH(c) + `</p></div></td></tr>`
	}

	cedula := ""
	if c := str(d["cedula"]); c != "" {
		cedula = `<p style="margin:4px 0 0; font-size:13px; color:#555;"><b>RUC / Cédula:</b> ` + escH(c) + `</p>`
	}

	color := "#e67e22"
	if tipoPago == "efectivo" {
		color = "#27ae60"
	} else if tipoPago == "transferencia" {
		color = "#2980b9"
	}

	return strings.NewReplacer(
		"{{FECHA}}", fechaLarga(time.Now()),
		"{{CLIENTE}}", escH(strOr(d["cliente"], "Consumidor Final")),
		"{{CEDULA}}", cedula,
		"{{COLOR}}", color,
		"{{TIPO}}", escH(tipoPago),
		"{{FILAS}}", filas.String(),
		"{{DESCUENTO}}", descuento,
		"{{DETALLE}}", detalle,
		"{{SRI}}", sri,
		"{{TOTAL}}", money(d["totalFinal"]),
	).Replace(tplFactura)
}

const tplAbono = `<!DOCTYPE html><html lang="es"><head><meta charset="UTF-8"></head>
<body style="margin:0;padding:0;background-color:#f5f6fa;font-family:'Segoe UI',Arial,sans-serif;">
<table width="100%" cellpadding="0" cellspacing="0" style="background-color:#f5f6fa;padding:30px 0;">
<tr><td align="center">
<table width="500" cellpadding="0" cellspacing="0" style="background-color:#ffffff;border-radius:12px;overflow:hidden;box-shadow:0 4px 15px rgba(0,0,0,0.06);">
<tr><td style="background-color:#27ae60;padding:24px 30px;text-align:center;">
<h1 style="margin:0;color:#ffffff;font-size:22px;">AGRO NARANJITO #1</h1>
<p style="margin:4px 0 0;color:rgba(255,255,255,0.85);font-size:13px;">Comprobante de Abono · {{FECHA}}</p>
</td></tr>
<tr><td style="padding:24px 30px;">
<p style="margin:0 0 6px;font-size:12px;color:#888;text-transform:uppercase;">Cliente</p>
<p style="margin:0 0 16px;font-size:17px;font-weight:600;color:#1e272e;">{{CLIENTE}}</p>
<table width="100%" cellpadding="0" cellspacing="0">
<tr><td style="padding:4px 0;color:#555;">Monto Abonado</td><td style="text-align:right;font-size:18px;font-weight:700;color:#27ae60;">${{MONTO}}</td></tr>
{{DETALLE}}
<tr><td colspan="2"><hr style="border:none;border-top:1px dashed #eee;margin:10px 0;"></td></tr>
<tr><td style="color:#555;">Deuda Total</td><td style="text-align:right;">${{TOTAL}}</td></tr>
<tr><td style="color:#555;">Total Pagado</td><td style="text-align:right;">${{PAGADO}}</td></tr>
<tr><td style="font-weight:700;">Saldo Restante</td><td style="text-align:right;font-weight:700;color:#ff3f34;">${{RESTANTE}}</td></tr>
</table>
{{ESTADO}}
</td></tr>
<tr><td style="background-color:#f8f9fa;padding:18px 30px;text-align:center;border-top:1px solid #f0f0f0;">
<p style="margin:0;font-size:13px;color:#2c3e50;">¡Gracias por su pago! 😊</p>
<p style="margin:4px 0 0;font-size:11px;color:#7f8c8d;">Agro Naranjito #1 · Comprobante automático de abono.</p>
</td></tr>
</table>
</td></tr>
</table>
</body></html>`

func generarHTMLCorreoAbono(d M) string {
	estado := ""
	if num(d["restante"]) <= 0 {
		estado = `<div style="background:#eafaf1;border:1px solid #27ae60;border-radius:6px;padding:10px 14px;text-align:center;margin-top:10px;"><p style="margin:0;color:#27ae60;font-weight:700;">✅ Deuda saldada por completo. ¡Gracias!</p></div>`
	}
	detalle := ""
	if str(d["tipoPago"]) == "transferencia" {
		detalle = filaPago("Entidad Bancaria", escH(strOr(d["banco"], "-")), " font-weight:500;") +
			filaPago("Nº Comprobante", escH(strOr(d["comprobante"], "-")), " font-weight:bold;")
	}
	return strings.NewReplacer(
		"{{FECHA}}", fechaLarga(time.Now()),
		"{{CLIENTE}}", escH(strOr(d["cliente"], "Cliente")),
		"{{MONTO}}", money(d["monto"]),
		"{{DETALLE}}", detalle,
		"{{TOTAL}}", money(d["total"]),
		"{{PAGADO}}", money(d["pagado"]),
		"{{RESTANTE}}", money(d["restante"]),
		"{{ESTADO}}", estado,
	).Replace(tplAbono)
}

// =========================================================================
// GUARDIÁN DE CADUCIDADES
// =========================================================================

const tplGuardian = `<!DOCTYPE html>
<html><head><meta charset="UTF-8"></head>
<body style="font-family:'Segoe UI',sans-serif; background:#f4f6f9; padding:20px; color:#333;">
<div style="max-width:650px; background:white; margin:0 auto; border-radius:12px; overflow:hidden; box-shadow:0 4px 10px rgba(0,0,0,0.05); border-top:5px solid #ff3f34;">
<div style="background:#1e272e; padding:20px; text-align:center; color:white;">
<h2 style="margin:0;">🚨 NEXUS — REPORTE DE CADUCIDADES</h2>
<p style="margin:5px 0 0; color:#aaa; font-size:14px;">Control de vencimientos automático para Agro Naranjito #1</p>
</div>
<div style="padding:24px;">
<p>Estimado Administrador, se han localizado las siguientes alertas prioritarias en su bodega:</p>
<table width="100%" cellpadding="0" cellspacing="0" style="border-collapse:collapse; margin-top:15px;">
<thead><tr style="background:#f8f9fa; border-bottom:2px solid #ddd; font-size:13px; color:#555;">
<th style="padding:10px; text-align:left;">Producto</th>
<th style="padding:10px; text-align:center;">Stock</th>
<th style="padding:10px; text-align:center;">Fecha Venc.</th>
<th style="padding:10px; text-align:right;">Estado</th>
</tr></thead>
<tbody>{{FILAS}}</tbody>
</table>
<p style="margin-top:25px; font-size:13px; color:#7f8c8d; text-align:center;">Nexus Core System · Este correo se genera automáticamente cada 24 horas.</p>
</div></div></body></html>`

type itemCad struct {
	nombre, codigo, stock, fecha string
	dias                         int
}

func filasCad(arr []itemCad, color, texto string) string {
	var sb strings.Builder
	for _, i := range arr {
		d := fmt.Sprint(i.dias)
		if i.dias < 0 {
			d = fmt.Sprintf("Hace %d", -i.dias)
		}
		sb.WriteString(`<tr>
<td style="padding:10px; border-bottom:1px solid #eee; text-align:left;"><b>` + escH(i.nombre) + `</b><br><small style="color:#777;">Cód: ` + escH(i.codigo) + `</small></td>
<td style="padding:10px; border-bottom:1px solid #eee; text-align:center; font-weight:bold;">` + escH(i.stock) + ` un.</td>
<td style="padding:10px; border-bottom:1px solid #eee; text-align:center; font-family:monospace;">` + escH(i.fecha) + `</td>
<td style="padding:10px; border-bottom:1px solid #eee; text-align:right;"><span style="background:` + color + `; color:white; padding:3px 8px; border-radius:5px; font-size:12px; font-weight:bold;">` + texto + ` (` + d + ` d)</span></td>
</tr>`)
	}
	return sb.String()
}

func ejecutarRevisionCaducidades() {
	if brevoKey == "" {
		log.Println("⚠️ Guardián abortado: Brevo API no configurada.")
		return
	}
	log.Println("🕒 Guardián de Inventario: Iniciando escaneo diario de caducidades...")

	docs, err := db.Collection("productos").Documents(ctx).GetAll()
	if err != nil {
		log.Println("❌ Fallo en el proceso automatizado del Guardián:", err)
		return
	}
	now := time.Now().In(loc)
	hoy := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	var vencidos, criticos, preventivos []itemCad
	for _, doc := range docs {
		p := doc.Data()
		cad := str(p["caducidad"])
		if cad == "" {
			continue
		}
		f, err := time.ParseInLocation("2006-01-02", cad, loc)
		if err != nil {
			continue
		}
		dias := int(math.Ceil(f.Sub(hoy).Hours() / 24))
		it := itemCad{
			nombre: strOr(p["nombre"], "Sin Nombre"),
			codigo: strOr(p["codigo"], "-"),
			stock:  strOr(p["stock"], "0"),
			fecha:  cad,
			dias:   dias,
		}
		switch {
		case dias < 0:
			vencidos = append(vencidos, it)
		case dias <= 30:
			criticos = append(criticos, it)
		case dias <= 90:
			preventivos = append(preventivos, it)
		}
	}

	if len(vencidos)+len(criticos)+len(preventivos) == 0 {
		log.Println("✅ Guardián de Inventario: Cero productos en riesgo de caducidad hoy.")
		return
	}

	filas := filasCad(vencidos, "#e74c3c", "⚠️ Vencido") +
		filasCad(criticos, "#e67e22", "⏳ Crítico") +
		filasCad(preventivos, "#f1c40f", "🕒 3 Meses")

	ok, e := enviarCorreoBrevo(
		envOr("ALERT_EMAIL", defaultBrevoEmail),
		fmt.Sprintf("🚨 ALERTA BODEGA: %d Vencidos / %d Críticos detectados", len(vencidos), len(criticos)),
		strings.Replace(tplGuardian, "{{FILAS}}", filas, 1),
		"NEXUS Guardián", "")
	if ok {
		log.Println("📧 Correo consolidado de alertas enviado exitosamente al administrador.")
	} else {
		log.Println("❌ No se pudo enviar el correo del guardián:", e)
	}
}

func iniciarGuardianCaducidades() {
	go func() {
		time.Sleep(5 * time.Second)
		ejecutarRevisionCaducidades()
		t := time.NewTicker(24 * time.Hour)
		for range t.C {
			ejecutarRevisionCaducidades()
		}
	}()
}

// ═════════════ inventario.go ═════════════
// =========================================================================
// PRODUCTOS
// =========================================================================

func getProductos(w http.ResponseWriter, r *http.Request) {
	docs, err := db.Collection("productos").Documents(ctx).GetAll()
	if err != nil {
		log.Println("Error al obtener productos:", err)
		send(w, []M{})
		return
	}
	send(w, mapDocs(docs))
}

func postProducto(w http.ResponseWriter, r *http.Request) {
	b := readBody(w, r)
	_, _, err := db.Collection("productos").Add(ctx, M{
		"codigo":       str(b["codigo"]),
		"nombre":       strOr(b["nombre"], "Sin Nombre"),
		"precioCompra": num(b["precioCompra"]),
		"precioVenta":  num(b["precioVenta"]),
		"stock":        num(b["stock"]),
		"caducidad":    nilIfEmpty(b["caducidad"]),
		"percha":       nilIfEmpty(b["percha"]),
	})
	if err != nil {
		fail(w, 500, "Error interno al crear producto")
		return
	}
	send(w, M{"ok": true})
}

func putProducto(w http.ResponseWriter, r *http.Request) {
	b := readBody(w, r)
	up := M{}
	for _, k := range []string{"codigo", "nombre"} {
		if v, ok := b[k]; ok {
			up[k] = v
		}
	}
	for _, k := range []string{"precioCompra", "precioVenta", "stock"} {
		if v, ok := b[k]; ok {
			up[k] = num(v)
		}
	}
	for _, k := range []string{"caducidad", "percha"} {
		if v, ok := b[k]; ok {
			up[k] = nilIfEmpty(v)
		}
	}
	if err := updateMap(db.Collection("productos").Doc(r.PathValue("id")), up); err != nil {
		fail(w, 500, "Error interno al editar propiedades del producto")
		return
	}
	send(w, M{"ok": true})
}

func agregarStock(w http.ResponseWriter, r *http.Request) {
	b := readBody(w, r)
	ref := db.Collection("productos").Doc(r.PathValue("id"))
	snap, existe, err := getDoc(ref)
	if err != nil {
		fail(w, 500, "Error interno al reaprovisionar stock")
		return
	}
	if !existe {
		fail(w, 404, "Documento objetivo inexistente")
		return
	}
	p := snap.Data()
	cantidad := num(b["cantidad"])

	ups := []firestore.Update{{Path: "stock", Value: firestore.Increment(cantidad)}}
	if v, ok := b["caducidad"]; ok {
		ups = append(ups, firestore.Update{Path: "caducidad", Value: v})
	}
	if _, err := ref.Update(ctx, ups); err != nil {
		fail(w, 500, "Error interno al reaprovisionar stock")
		return
	}
	if err := registrarMovimiento("entrada", strOr(p["codigo"], "-"), strOr(p["nombre"], "Sin Nombre"),
		cantidad, strOr(b["motivo"], "Reabastecimiento de bodega")); err != nil {
		fail(w, 500, "Error interno al reaprovisionar stock")
		return
	}
	send(w, M{"ok": true})
}

func venderProducto(w http.ResponseWriter, r *http.Request) {
	b := readBody(w, r)
	ref := db.Collection("productos").Doc(r.PathValue("id"))
	snap, existe, err := getDoc(ref)
	if err != nil {
		fail(w, 500, "Error de inventario al procesar el descuento posventa")
		return
	}
	if !existe {
		fail(w, 404, "El producto no existe en el catálogo")
		return
	}
	p := snap.Data()
	cantidad := num(b["cantidad"])
	nuevo := num(p["stock"]) - cantidad
	faltante := 0.0
	if nuevo < 0 {
		faltante = -nuevo
		nuevo = 0
	}
	if _, err := ref.Update(ctx, []firestore.Update{{Path: "stock", Value: nuevo}}); err != nil {
		fail(w, 500, "Error de inventario al procesar el descuento posventa")
		return
	}
	if faltante > 0 {
		log.Printf("⚠️ Venta de %q superó el stock en %v unidad(es). Stock dejado en 0.", p["nombre"], faltante)
	}
	send(w, M{"ok": true, "stock": nuevo, "sobreventa": faltante})
}

func deleteProducto(w http.ResponseWriter, r *http.Request) {
	if _, err := db.Collection("productos").Doc(r.PathValue("id")).Delete(ctx); err != nil {
		fail(w, 500, "Error interno al purgar producto")
		return
	}
	send(w, M{"ok": true})
}

// =========================================================================
// MOVIMIENTOS DE INVENTARIO
// =========================================================================

func getMovimientos(w http.ResponseWriter, r *http.Request) {
	docs, err := db.Collection("movimientos-inventario").OrderBy("fecha", firestore.Desc).Documents(ctx).GetAll()
	if err != nil {
		log.Println("❌ Error al obtener movimientos de inventario:", err)
		writeJSON(w, 500, []M{})
		return
	}
	send(w, mapDocs(docs))
}

func postMovimiento(w http.ResponseWriter, r *http.Request) {
	b := readBody(w, r)
	now := time.Now().In(loc)
	_, _, err := db.Collection("movimientos-inventario").Add(ctx, M{
		"tipo":     strOr(b["tipo"], "entrada"),
		"codigo":   strOr(b["codigo"], "-"),
		"nombre":   strOr(b["nombre"], "Sin Nombre"),
		"cantidad": num(b["cantidad"]),
		"fecha":    strOr(b["fecha"], now.Format("2006-01-02")),
		"hora":     strOr(b["hora"], now.Format("15:04:05")),
		"motivo":   strOr(b["motivo"], "Actualización manual"),
	})
	if err != nil {
		log.Println("❌ Error al guardar movimiento en Firestore:", err)
		fail(w, 500, "Error interno al guardar el historial")
		return
	}
	send(w, M{"ok": true, "mensaje": "Movimiento asentado en auditoría"})
}

func getCaducidades(w http.ResponseWriter, r *http.Request) {
	docs, err := db.Collection("productos").Documents(ctx).GetAll()
	if err != nil {
		log.Println("❌ Error al leer caducidades:", err)
		fail(w, 500, "Error al obtener caducidades de Firestore")
		return
	}
	out := []M{}
	for _, d := range docs {
		p := d.Data()
		if str(p["caducidad"]) == "" {
			continue
		}
		stock := p["stock"]
		if stock == nil {
			stock = 0
		}
		out = append(out, M{
			"_id":              d.Ref.ID,
			"nombre":           strOr(p["nombre"], "Sin Nombre"),
			"codigo":           strOr(p["codigo"], "-"),
			"fechaVencimiento": p["caducidad"],
			"stock":            stock,
			"precioVenta":      num(p["precioVenta"]),
			"precioCompra":     num(p["precioCompra"]),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return str(out[i]["fechaVencimiento"]) < str(out[j]["fechaVencimiento"])
	})
	send(w, out)
}

// ═════════════ clientes_ventas.go ═════════════
// =========================================================================
// CLIENTES
// =========================================================================

func postCliente(w http.ResponseWriter, r *http.Request) {
	b := readBody(w, r)
	nuevo := M{
		"nombre":      strOr(b["nombre"], "Sin Nombre"),
		"cedula":      strOr(b["cedula"], "Sin Cédula"),
		"direccion":   str(b["direccion"]),
		"telefono":    str(b["telefono"]),
		"correo":      str(b["correo"]),
		"deudaTotal":  num(b["deudaTotal"]),
		"deudaActual": num(b["deudaActual"]),
		"estado":      strOr(b["estado"], "normal"),
		"fecha":       fechaOrNow(b["fecha"]),
	}
	ref, _, err := db.Collection("clientes").Add(ctx, nuevo)
	if err != nil {
		log.Println("❌ Error registrando cliente:", err)
		failDet(w, 500, "Error de persistencia", err)
		return
	}
	nuevo["_id"] = ref.ID
	send(w, M{"ok": true, "cliente": nuevo})
}

func getClientes(w http.ResponseWriter, r *http.Request) {
	docs, err := db.Collection("clientes").OrderBy("fecha", firestore.Desc).Documents(ctx).GetAll()
	if err != nil {
		log.Println(err)
		writeJSON(w, 500, []M{})
		return
	}
	send(w, mapDocs(docs))
}

func clientePorCedula(cedula string) (*firestore.DocumentSnapshot, error) {
	docs, err := db.Collection("clientes").Where("cedula", "==", cedula).Documents(ctx).GetAll()
	if err != nil || len(docs) == 0 {
		return nil, err
	}
	return docs[0], nil
}

func sumarDeuda(w http.ResponseWriter, r *http.Request) {
	b := readBody(w, r)
	snap, err := clientePorCedula(str(b["cedula"]))
	if err != nil {
		fail(w, 500, "Error crítico al cargar línea de crédito")
		return
	}
	if snap == nil {
		fail(w, 404, "Cliente no registrado en la base de datos")
		return
	}
	c := snap.Data()
	total := num(b["total"])
	c["deudaTotal"] = num(c["deudaTotal"]) + total
	c["deudaActual"] = num(c["deudaActual"]) + total
	c["estado"] = "deudor"
	_, err = snap.Ref.Update(ctx, []firestore.Update{
		{Path: "deudaTotal", Value: c["deudaTotal"]},
		{Path: "deudaActual", Value: c["deudaActual"]},
		{Path: "estado", Value: "deudor"},
	})
	if err != nil {
		fail(w, 500, "Error crítico al cargar línea de crédito")
		return
	}
	c["_id"] = snap.Ref.ID
	send(w, M{"ok": true, "cliente": c})
}

func abonarCliente(w http.ResponseWriter, r *http.Request) {
	b := readBody(w, r)
	snap, err := clientePorCedula(str(b["cedula"]))
	if err != nil {
		fail(w, 500, "Error de red al aplicar abono parcial")
		return
	}
	if snap == nil {
		fail(w, 404, "Titular no encontrado")
		return
	}
	c := snap.Data()
	deuda := num(c["deudaActual"]) - num(b["monto"])
	estado := c["estado"]
	if deuda <= 0 {
		deuda = 0
		estado = "normal"
	}
	_, err = snap.Ref.Update(ctx, []firestore.Update{
		{Path: "deudaActual", Value: deuda},
		{Path: "estado", Value: estado},
	})
	if err != nil {
		fail(w, 500, "Error de red al aplicar abono parcial")
		return
	}
	c["deudaActual"], c["estado"], c["_id"] = deuda, estado, snap.Ref.ID
	send(w, M{"ok": true, "cliente": c})
}

func editarCliente(w http.ResponseWriter, r *http.Request) {
	b := readBody(w, r)
	err := updateMap(db.Collection("clientes").Doc(str(b["id"])), M{
		"nombre": b["nombre"], "cedula": b["cedula"], "telefono": b["telefono"], "correo": b["correo"],
	})
	if err != nil {
		fail(w, 500, "Error al regresar ficha de cliente")
		return
	}
	send(w, M{"ok": true})
}

func getClientePorCedula(w http.ResponseWriter, r *http.Request) {
	snap, err := clientePorCedula(r.PathValue("cedula"))
	if err != nil {
		writeJSON(w, 500, nil)
		return
	}
	if snap == nil {
		send(w, nil)
		return
	}
	c := snap.Data()
	c["_id"] = snap.Ref.ID
	send(w, c)
}

func deleteCliente(w http.ResponseWriter, r *http.Request) {
	if _, err := db.Collection("clientes").Doc(r.PathValue("id")).Delete(ctx); err != nil {
		fail(w, 500, "No se pudo eliminar el cliente seleccionado")
		return
	}
	send(w, M{"ok": true})
}

// =========================================================================
// CORREO
// =========================================================================

func asM(v any) M {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return M{}
}

func correoFactura(w http.ResponseWriter, r *http.Request) {
	b := readBody(w, r)
	correo := str(b["correo"])
	log.Println("📨 POST /correo/factura para:", correo)
	if correo == "" {
		fail(w, 400, "La dirección de correo destinataria es obligatoria.")
		return
	}
	if brevoKey == "" {
		fail(w, 503, "El servicio de correo (Brevo API) no está configurado.")
		return
	}
	datos := asM(b["datos"])
	html := generarHTMLCorreo(datos, asList(b["carrito"]), str(b["tipoPago"]))
	subject := fmt.Sprintf("🧾 Comprobante Digital — %s · Total: $%s", strOr(datos["cliente"], "Cliente"), money(datos["totalFinal"]))

	if ok, e := enviarCorreoBrevo(correo, subject, html, "Agro Naranjito #1", ""); !ok {
		log.Println("❌ Error crítico en Brevo API:", e)
		writeJSON(w, 500, M{"error": "Fallo crítico al despachar correo electrónico.", "detalle": e})
		return
	}
	send(w, M{"ok": true, "mensaje": "Correo enviado satisfactoriamente a " + correo})
}

func correoAbono(w http.ResponseWriter, r *http.Request) {
	b := readBody(w, r)
	correo := str(b["correo"])
	log.Println("📨 POST /correo/abono para:", correo)
	if correo == "" {
		fail(w, 400, "Correo destinatario obligatorio")
		return
	}
	if brevoKey == "" {
		fail(w, 503, "El servicio de correo (Brevo API) no está configurado.")
		return
	}
	datos := asM(b["datos"])
	subject := fmt.Sprintf("💰 Comprobante de Abono — %s · $%s", strOr(datos["cliente"], "Cliente"), money(datos["monto"]))
	if ok, e := enviarCorreoBrevo(correo, subject, generarHTMLCorreoAbono(datos), "Agro Naranjito #1", ""); !ok {
		log.Println("❌ Error al enviar correo de abono:", e)
		writeJSON(w, 500, M{"error": "Fallo al enviar correo de abono", "detalle": e})
		return
	}
	send(w, M{"ok": true, "mensaje": "Correo de abono enviado a " + correo})
}

// =========================================================================
// VENTAS
// =========================================================================

func postVenta(w http.ResponseWriter, r *http.Request) {
	b := readBody(w, r)
	errVenta := func(err error) {
		log.Println("❌ Error al asentar venta:", err)
		fail(w, 500, "Fallo del sistema al asentar venta")
	}

	venta := M{}
	for k, v := range b {
		venta[k] = v
	}
	venta["fecha"] = fechaOrNow(b["fecha"])
	ventaRef, _, err := db.Collection("ventas").Add(ctx, venta)
	if err != nil {
		errVenta(err)
		return
	}

	tipo := str(b["tipo"])
	productos := asList(b["productos"])

	if _, esArr := b["productos"].([]any); esArr {
		cliente := strOr(b["cliente"], "Consumidor Final")
		for _, p := range productos {
			motivo := "Venta — Cliente: " + cliente
			if tipo == "credito" {
				motivo = "[CREDITO] " + motivo
			}
			if err := registrarMovimiento("salida", strOr(p["codigo"], "-"), strOr(p["nombre"], "Sin Nombre"), cantidadDe(p), motivo); err != nil {
				errVenta(err)
				return
			}
		}
	}

	if tipo == "efectivo" || tipo == "transferencia" {
		caja, err := cajaActiva()
		if err != nil {
			errVenta(err)
			return
		}
		if caja != nil {
			total := num(b["total"])
			ahora := nowISO()
			var mov M
			if tipo == "efectivo" {
				mov = M{"tipo": "ingreso", "monto": total, "motivo": "Venta directa efectivo - Cliente: " + str(b["cliente"]), "fecha": ahora}
			} else {
				mov = M{
					"tipo": "transferencia", "monto": total,
					"motivo":      "Liquidación por Transferencia — " + str(b["banco"]),
					"banco":       str(b["banco"]),
					"cuenta":      str(b["cuenta"]),
					"comprobante": str(b["comprobante"]),
					"remitente":   str(b["cliente"]),
					"fecha":       ahora,
				}
			}
			if err := cajaRegistrar(caja, total, 0, mov); err != nil {
				errVenta(err)
				return
			}
		}
	}

	if tipo == "credito" {
		prods, _ := b["productos"].([]any)
		if prods == nil {
			prods = []any{}
		}
		_, _, err := db.Collection("deudas").Add(ctx, M{
			"cliente":     b["cliente"],
			"cedula":      strOr(b["cedula"], "SIN CÉDULA"),
			"celular":     str(b["celular"]),
			"correo":      str(b["correo"]),
			"direccion":   str(b["direccion"]),
			"total":       b["total"],
			"pagado":      0,
			"diasCredito": intOr(b["diasCredito"], 30),
			"productos":   prods,
			"pagos":       []any{},
			"fecha":       nowISO(),
		})
		if err != nil {
			errVenta(err)
			return
		}
	}

	var claveSRI any
	if tipo == "efectivo" || tipo == "transferencia" {
		sri := emitirFacturaInvoka(
			strOr(b["cliente"], "CONSUMIDOR FINAL"), str(b["cedula"]), str(b["correo"]),
			productos, num(b["descuento"]))
		if sri.OK {
			claveSRI = sri.ClaveAcceso
			_ = updateMap(ventaRef, M{"claveAccesoSRI": sri.ClaveAcceso, "autorizacionSRI": sri.Autorizacion, "facturaElectronica": true})
			log.Printf("🧾 Clave SRI guardada en venta %s", ventaRef.ID)
		} else {
			log.Printf("⚠️ Invoka no autorizó la factura para venta %s: %s", ventaRef.ID, sri.Err)
			_ = updateMap(ventaRef, M{"facturaElectronica": false, "errorSRI": strOr(sri.Err, "Sin detalle")})
		}
	}

	// 📧 Comprobante automático al correo del cliente (en segundo plano)
	if correo := str(b["correo"]); correo != "" && brevoKey != "" {
		datos := M{
			"cliente": b["cliente"], "cedula": b["cedula"],
			"subtotal": num(b["subtotal"]), "pct": num(b["descuentoPct"]), "descuentoMonto": num(b["descuentoMonto"]),
			"totalFinal": num(b["total"]), "tasaPct": num(b["tasaInteres"]), "montoInteres": num(b["montoInteres"]),
			"meses": num(b["meses"]), "pago": num(b["pago"]), "vuelto": num(b["vuelto"]),
			"bancoNombre": str(b["banco"]), "bancoCuenta": str(b["cuenta"]), "comprobante": str(b["comprobante"]),
			"claveAccesoSRI": str(claveSRI),
		}
		html := generarHTMLCorreo(datos, productos, tipo)
		subject := fmt.Sprintf("🧾 Comprobante Digital — %s · Total: $%s", strOr(datos["cliente"], "Cliente"), money(datos["totalFinal"]))
		go func() {
			ok, e := enviarCorreoBrevo(correo, subject, html, "Agro Naranjito #1", "")
			if ok {
				log.Println("📧 Resultado envío correo de venta: OK")
			} else {
				log.Println("📧 Resultado envío correo de venta:", e)
			}
		}()
	}

	send(w, M{"ok": true})
}

// intOr: Number(v || def)
func intOr(v any, def float64) float64 {
	if n := num(v); n != 0 {
		return n
	}
	return def
}

func deleteProductoVenta(w http.ResponseWriter, r *http.Request) {
	ref := db.Collection("ventas").Doc(r.PathValue("ventaId"))
	snap, existe, err := getDoc(ref)
	if err != nil {
		fail(w, 500, "Error al modificar la venta consolidada")
		return
	}
	if !existe {
		fail(w, 404, "Registro de transacción no localizado")
		return
	}
	venta := snap.Data()
	prods, _ := venta["productos"].([]any)
	idx, convErr := strconv.Atoi(r.PathValue("indice"))
	if convErr != nil || idx < 0 || idx >= len(prods) {
		fail(w, 400, "Direccionamiento indexado incorrecto")
		return
	}
	prods = append(prods[:idx:idx], prods[idx+1:]...)

	if len(prods) == 0 {
		if _, err := ref.Delete(ctx); err != nil {
			fail(w, 500, "Error al modificar la venta consolidada")
			return
		}
		send(w, M{"msg": "Transacción purgada en su totalidad"})
		return
	}
	total := 0.0
	for _, p := range prods {
		if m, ok := p.(map[string]any); ok {
			total += num(m["precio"]) * cantidadDe(m)
		}
	}
	if err := updateMap(ref, M{"productos": prods, "total": total}); err != nil {
		fail(w, 500, "Error al modificar la venta consolidada")
		return
	}
	send(w, M{"msg": "Item removido e importes recalculados."})
}

func deleteVentasDia(w http.ResponseWriter, r *http.Request) {
	b := readBody(w, r)
	fecha := str(b["fecha"])
	if fecha == "" {
		fail(w, 400, "Parámetro fecha ausente")
		return
	}
	docs, err := db.Collection("ventas").
		Where("fecha", ">=", fecha+"T00:00:00.000Z").
		Where("fecha", "<=", fecha+"T23:59:59.999Z").
		Documents(ctx).GetAll()
	if err != nil {
		fail(w, 500, "Fallo al purgar registros diarios")
		return
	}
	for i := 0; i < len(docs); i += 400 {
		fin := i + 400
		if fin > len(docs) {
			fin = len(docs)
		}
		batch := db.Batch()
		for _, d := range docs[i:fin] {
			batch.Delete(d.Ref)
		}
		if _, err := batch.Commit(ctx); err != nil {
			fail(w, 500, "Fallo al purgar registros diarios")
			return
		}
	}
	send(w, M{"ok": true, "msg": fmt.Sprintf("Cierre forzado: %d venta(s) eliminada(s)", len(docs)), "deleted": len(docs)})
}

// =========================================================================
// DEUDAS
// =========================================================================

func getDeudas(w http.ResponseWriter, r *http.Request) {
	docs, err := db.Collection("deudas").OrderBy("fecha", firestore.Desc).Documents(ctx).GetAll()
	if err != nil {
		send(w, []M{})
		return
	}
	send(w, mapDocs(docs))
}

func postDeuda(w http.ResponseWriter, r *http.Request) {
	b := readBody(w, r)
	prods, _ := b["productos"].([]any)
	if prods == nil {
		prods = []any{}
	}
	nueva := M{
		"cliente":     str(b["cliente"]),
		"cedula":      strOr(b["cedula"], "-"),
		"celular":     str(b["celular"]),
		"direccion":   str(b["direccion"]),
		"correo":      str(b["correo"]),
		"total":       num(b["total"]),
		"pagado":      0,
		"diasCredito": intOr(b["diasCredito"], 30),
		"productos":   prods,
		"pagos":       []any{},
		"fecha":       fechaOrNow(b["fecha"]),
	}
	ref, _, err := db.Collection("deudas").Add(ctx, nueva)
	if err != nil {
		fail(w, 500, "No se pudo aperturar la cuenta por cobrar")
		return
	}
	nueva["_id"] = ref.ID
	send(w, nueva)
}

var (
	errNoExiste  = errors.New("NO_EXISTE")
	errSobrepago = errors.New("SOBREPAGO")
)

func pagarDeuda(w http.ResponseWriter, r *http.Request) {
	b := readBody(w, r)
	ref := db.Collection("deudas").Doc(str(b["id"]))
	monto := num(b["monto"])
	if monto <= 0 {
		send(w, M{"error": "Importe introducido inválido"})
		return
	}
	metodo := strOr(b["tipoPago"], strOr(b["metodoPago"], "efectivo"))
	banco, comprobante, remitente := str(b["banco"]), str(b["comprobante"]), str(b["remitente"])
	pagoNuevo := M{"monto": monto, "tipoPago": metodo, "banco": banco, "comprobante": comprobante, "remitente": remitente, "fecha": nowISO()}

	// Transacción: validación de saldo y registro del pago van juntos
	var deuda M
	err := db.RunTransaction(ctx, func(_ context.Context, tx *firestore.Transaction) error {
		snap, err := tx.Get(ref)
		if status.Code(err) == codes.NotFound {
			return errNoExiste
		}
		if err != nil {
			return err
		}
		d := snap.Data()
		restante := num(d["total"]) - num(d["pagado"])
		if monto > restante+0.005 {
			return errSobrepago
		}
		pagos, _ := d["pagos"].([]any)
		d["pagado"] = num(d["pagado"]) + monto
		d["pagos"] = append(append([]any{}, pagos...), pagoNuevo)
		deuda = d
		return tx.Update(ref, []firestore.Update{
			{Path: "pagado", Value: d["pagado"]},
			{Path: "pagos", Value: d["pagos"]},
		})
	})
	switch {
	case errors.Is(err, errNoExiste):
		send(w, M{"error": "Cuenta de deuda no localizada"})
		return
	case errors.Is(err, errSobrepago):
		send(w, M{"error": "Sobrepago no permitido para el saldo restante"})
		return
	case err != nil:
		log.Println("❌ Error al asentar amortización:", err)
		fail(w, 500, "Fallo crítico al asentar amortización")
		return
	}

	if caja, _ := cajaActiva(); caja != nil {
		ahora := nowISO()
		var mov M
		if metodo == "transferencia" {
			mov = M{
				"tipo": "transferencia", "monto": monto,
				"motivo": "Abono a Cuenta Diferida — " + str(deuda["cliente"]),
				"banco":  banco, "comprobante": comprobante,
				"remitente": strOr(remitente, str(deuda["cliente"])),
				"fecha":     ahora,
			}
		} else {
			mov = M{"tipo": "ingreso", "monto": monto, "motivo": "Abono Efectivo Deuda — " + str(deuda["cliente"]), "fecha": ahora}
		}
		if err := cajaRegistrar(caja, monto, 0, mov); err != nil {
			log.Println("❌ Error al asentar amortización en caja:", err)
			fail(w, 500, "Fallo crítico al asentar amortización")
			return
		}
	}

	restanteFinal := num(deuda["total"]) - num(deuda["pagado"])

	if correo := str(deuda["correo"]); correo != "" && brevoKey != "" {
		html := generarHTMLCorreoAbono(M{
			"cliente": deuda["cliente"], "monto": monto, "total": deuda["total"], "pagado": deuda["pagado"],
			"restante": restanteFinal, "tipoPago": metodo, "banco": banco, "comprobante": comprobante, "remitente": remitente,
		})
		subject := fmt.Sprintf("💰 Comprobante de Abono — %s · $%.2f", strOr(deuda["cliente"], "Cliente"), monto)
		go func() {
			ok, e := enviarCorreoBrevo(correo, subject, html, "Agro Naranjito #1", "")
			if ok {
				log.Println("📧 Resultado envío correo de abono: OK")
			} else {
				log.Println("📧 Resultado envío correo de abono:", e)
			}
		}()
	}

	productos := deuda["productos"]
	if productos == nil {
		productos = []any{}
	}
	send(w, M{
		"cliente": deuda["cliente"], "cedula": strOr(deuda["cedula"], "-"), "celular": str(deuda["celular"]),
		"monto": monto, "total": deuda["total"], "restante": restanteFinal, "pagado": deuda["pagado"],
		"pagos": deuda["pagos"], "productos": productos,
	})
}

func putDeuda(w http.ResponseWriter, r *http.Request) {
	b := readBody(w, r)
	ref := db.Collection("deudas").Doc(r.PathValue("id"))
	snap, existe, err := getDoc(ref)
	if err != nil {
		fail(w, 500, "Fallo técnico al editar balance de deuda")
		return
	}
	if !existe {
		fail(w, 404, "Cuenta no encontrada")
		return
	}
	deuda := snap.Data()
	for _, k := range []string{"cliente", "cedula", "celular", "direccion", "correo", "productos", "pagos"} {
		if v, ok := b[k]; ok {
			deuda[k] = v
		}
	}
	for _, k := range []string{"total", "pagado", "diasCredito"} {
		if v, ok := b[k]; ok {
			deuda[k] = num(v)
		}
	}
	if err := updateMap(ref, deuda); err != nil {
		fail(w, 500, "Fallo técnico al editar balance de deuda")
		return
	}
	deuda["_id"] = ref.ID
	send(w, M{"ok": true, "deuda": deuda})
}

func deleteDeuda(w http.ResponseWriter, r *http.Request) {
	if _, err := db.Collection("deudas").Doc(r.PathValue("id")).Delete(ctx); err != nil {
		fail(w, 500, "Error de borrado")
		return
	}
	send(w, M{"ok": true})
}

// ═════════════ pedidos_riesgo.go ═════════════
// =========================================================================
// PEDIDOS A PROVEEDORES (PREFORMAS)
// =========================================================================

func normTxt(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(s))), " ")
}

func getPedidos(w http.ResponseWriter, r *http.Request) {
	docs, err := db.Collection("pedidos").OrderBy("fechaPreforma", firestore.Desc).Documents(ctx).GetAll()
	if err != nil {
		log.Println("❌ Error al obtener pedidos:", err)
		writeJSON(w, 500, []M{})
		return
	}
	send(w, mapDocs(docs))
}

func postPedido(w http.ResponseWriter, r *http.Request) {
	b := readBody(w, r)
	productos := []any{}
	total := 0.0
	for _, p := range asList(b["productos"]) {
		cant, precio := num(p["cantidadPedida"]), num(p["precioUnitario"])
		total += cant * precio
		var pid any
		if s := str(p["productoId"]); s != "" {
			pid = s
		}
		productos = append(productos, M{
			"nombre":           strOr(p["nombre"], "Sin Nombre"),
			"productoId":       pid,
			"codigo":           str(p["codigo"]),
			"medida":           strOr(p["medida"], "-"),
			"cantidadPedida":   cant,
			"precioUnitario":   precio,
			"cantidadRecibida": nil,
		})
	}
	nuevo := M{
		"quienPide":     str(b["quienPide"]),
		"proveedor":     strings.TrimSpace(strOr(b["proveedor"], "Sin especificar")),
		"productos":     productos,
		"totalPreforma": round2(total), // el total se recalcula en el servidor
		"estado":        "pendiente",
		"fechaPreforma": nowISO(),
		"fechaLlegada":  nil,
	}
	ref, _, err := db.Collection("pedidos").Add(ctx, nuevo)
	if err != nil {
		log.Println("❌ Error al crear pedido:", err)
		fail(w, 500, "No se pudo registrar la preforma")
		return
	}
	nuevo["_id"] = ref.ID
	send(w, M{"ok": true, "pedido": nuevo})
}

// Busca el producto de Bodega: primero por ID, luego por nombre normalizado.
func localizarProductoBodega(it M, cache []*firestore.DocumentSnapshot) *firestore.DocumentSnapshot {
	if id := str(it["productoId"]); id != "" {
		if d, ok, _ := getDoc(db.Collection("productos").Doc(id)); ok {
			return d
		}
	}
	n := normTxt(str(it["nombre"]))
	for _, d := range cache {
		if normTxt(str(d.Data()["nombre"])) == n {
			return d
		}
	}
	return nil
}

func recibirPedido(w http.ResponseWriter, r *http.Request) {
	b := readBody(w, r)
	ref := db.Collection("pedidos").Doc(r.PathValue("id"))
	snap, existe, err := getDoc(ref)
	if err != nil {
		fail(w, 500, "No se pudo registrar la llegada")
		return
	}
	if !existe {
		fail(w, 404, "Pedido no encontrado")
		return
	}
	pedido := snap.Data()
	previos := asList(pedido["productos"])
	enviados := previos
	if _, ok := b["productos"].([]any); ok {
		enviados = asList(b["productos"])
	}

	cache, err := db.Collection("productos").Documents(ctx).GetAll()
	if err != nil {
		fail(w, 500, "No se pudo registrar la llegada")
		return
	}

	productos := []any{}
	noEncontrados := []string{}
	proveedor := strOr(pedido["proveedor"], "proveedor")

	for i, prev := range previos {
		nuevo := prev
		if i < len(enviados) {
			nuevo = enviados[i]
		}
		yaRecibido := num(prev["cantidadRecibida"])
		pedida := num(prev["cantidadPedida"])

		// Nunca más de lo pedido ni retroceder lo ya recibido
		pedidoRecibido := yaRecibido
		if v, ok := nuevo["cantidadRecibida"]; ok && v != nil {
			pedidoRecibido = num(v)
		}
		recibidaTotal := math.Min(pedida, math.Max(yaRecibido, pedidoRecibido))
		delta := recibidaTotal - yaRecibido // solo se suma lo NUEVO

		// Precio real de la factura
		if v, ok := nuevo["precioUnitario"]; ok && v != nil && str(v) != "" {
			if pn := num(v); pn >= 0 {
				prev["precioUnitario"] = math.Round(pn*10000) / 10000
			}
		}

		if delta > 0 {
			if prod := localizarProductoBodega(prev, cache); prod != nil {
				ups := []firestore.Update{{Path: "stock", Value: firestore.Increment(delta)}}
				if pu := num(prev["precioUnitario"]); pu > 0 {
					ups = append(ups, firestore.Update{Path: "precioCompra", Value: pu})
				}
				if _, err := prod.Ref.Update(ctx, ups); err != nil {
					fail(w, 500, "No se pudo registrar la llegada")
					return
				}
				pd := prod.Data()
				_ = registrarMovimiento("entrada", strOr(pd["codigo"], "-"), strOr(pd["nombre"], str(prev["nombre"])),
					delta, "Llegada de pedido — "+proveedor)
			} else {
				noEncontrados = append(noEncontrados, str(prev["nombre"]))
			}
		}
		prev["cantidadRecibida"] = recibidaTotal
		productos = append(productos, prev)
	}

	completo := true
	total := 0.0
	for _, p := range previos {
		if num(p["cantidadRecibida"]) < num(p["cantidadPedida"]) {
			completo = false
		}
		total += num(p["cantidadPedida"]) * num(p["precioUnitario"])
	}
	estado := "incompleto"
	if completo {
		estado = "completo"
	}
	total = round2(total)

	err = updateMap(ref, M{"productos": productos, "estado": estado, "totalPreforma": total, "fechaLlegada": nowISO()})
	if err != nil {
		log.Println("❌ Error al registrar llegada del pedido:", err)
		fail(w, 500, "No se pudo registrar la llegada")
		return
	}
	send(w, M{"ok": true, "estado": estado, "noEncontrados": noEncontrados, "totalPreforma": total})
}

func deletePedido(w http.ResponseWriter, r *http.Request) {
	ref := db.Collection("pedidos").Doc(r.PathValue("id"))
	snap, existe, err := getDoc(ref)
	if err != nil {
		fail(w, 500, "No se pudo eliminar el pedido")
		return
	}
	if !existe {
		fail(w, 404, "Pedido no encontrado")
		return
	}
	d := snap.Data()
	yaEntro := false
	for _, p := range asList(d["productos"]) {
		if num(p["cantidadRecibida"]) > 0 {
			yaEntro = true
		}
	}
	// Solo se bloquea un pedido con llegada PARCIAL
	if yaEntro && str(d["estado"]) != "completo" {
		fail(w, 400, "Tiene una llegada parcial. Completa la llegada antes de eliminarlo.")
		return
	}
	if _, err := ref.Delete(ctx); err != nil {
		fail(w, 500, "No se pudo eliminar el pedido")
		return
	}
	send(w, M{"ok": true})
}

// =========================================================================
// RIESGO CREDITICIO
// =========================================================================

const dia = 24 * time.Hour

func calcularScoreRiesgo(deudas []M) (score int, nivel string, atrasoMax int, vencidasAbiertas int) {
	s := 100.0
	hoy := time.Now()
	maxAtraso := 0.0

	for _, d := range deudas {
		diasCredito := intOr(d["diasCredito"], 30)
		creacion, ok := parseTime(d["fecha"])
		if !ok {
			continue
		}
		venc := creacion.Add(time.Duration(diasCredito) * dia)
		pendiente := num(d["total"]) - num(d["pagado"])

		if pendiente <= 0.01 {
			pagos := asList(d["pagos"])
			if len(pagos) > 0 {
				if ultimo, ok := parseTime(pagos[len(pagos)-1]["fecha"]); ok && ultimo.After(venc) {
					tarde := math.Ceil(float64(ultimo.Sub(venc)) / float64(dia))
					s -= math.Min(15, 3+tarde*0.3)
					maxAtraso = math.Max(maxAtraso, tarde)
				}
			}
		} else if hoy.After(venc) {
			tarde := math.Ceil(float64(hoy.Sub(venc)) / float64(dia))
			s -= math.Min(40, 5+tarde*0.5)
			maxAtraso = math.Max(maxAtraso, tarde)
			vencidasAbiertas++
		}
	}

	score = int(math.Round(math.Max(0, math.Min(100, s))))
	switch {
	case score >= 80:
		nivel = "bajo"
	case score >= 50:
		nivel = "medio"
	default:
		nivel = "alto"
	}
	return score, nivel, int(maxAtraso), vencidasAbiertas
}

func getRiesgo(w http.ResponseWriter, r *http.Request) {
	cs, err1 := db.Collection("clientes").Documents(ctx).GetAll()
	ds, err2 := db.Collection("deudas").Documents(ctx).GetAll()
	if err1 != nil || err2 != nil {
		log.Println("❌ Error al calcular riesgo:", err1, err2)
		fail(w, 500, "Error al calcular el riesgo crediticio")
		return
	}
	clientes, deudas := mapDocs(cs), mapDocs(ds)

	resultado := []M{}
	for _, c := range clientes {
		var dc []M
		for _, d := range deudas {
			if str(d["cedula"]) == str(c["cedula"]) {
				dc = append(dc, d)
			}
		}
		if len(dc) == 0 {
			continue
		}
		score, nivel, atraso, venc := calcularScoreRiesgo(dc)
		hist, abonado := 0.0, 0.0
		for _, d := range dc {
			hist += num(d["total"])
			abonado += num(d["pagado"])
		}
		resultado = append(resultado, M{
			"_id": c["_id"], "nombre": c["nombre"], "cedula": c["cedula"], "telefono": str(c["telefono"]),
			"totalCreditoHistorico": round2(hist), "totalAbonado": round2(abonado), "deudaActual": round2(hist - abonado),
			"score": score, "nivel": nivel, "diasAtrasoMax": atraso, "deudasVencidasAbiertas": venc,
			"cantidadDeudas": len(dc),
		})
	}
	sort.SliceStable(resultado, func(i, j int) bool { return resultado[i]["score"].(int) < resultado[j]["score"].(int) })
	send(w, resultado)
}

func getRiesgoResumen(w http.ResponseWriter, r *http.Request) {
	ds, err := db.Collection("deudas").Documents(ctx).GetAll()
	if err != nil {
		log.Println("❌ Error en resumen de riesgo:", err)
		fail(w, 500, "Error al calcular resumen de riesgo")
		return
	}
	total, abonado := 0.0, 0.0
	for _, d := range mapDocs(ds) {
		total += num(d["total"])
		abonado += num(d["pagado"])
	}
	pct := 0.0
	if total > 0 {
		pct = math.Round(abonado/total*1000) / 10
	}
	send(w, M{"totalCredito": round2(total), "totalAbonado": round2(abonado), "totalPendiente": round2(total - abonado), "pctRecuperacion": pct})
}

// ═════════════ caja_analisis.go ═════════════
// =========================================================================
// CAJA
// =========================================================================

type entrada struct {
	ts time.Time
	m  M
}

func entradaM(tipo, producto string, cantidad float64, fecha any, nota string) M {
	return M{"tipo": tipo, "producto": producto, "cantidad": cantidad, "fecha": fecha, "horaRegistro": horaLocal(fecha), "nota": nota}
}

func ordenar(es []entrada) []M {
	sort.SliceStable(es, func(i, j int) bool { return es[i].ts.Before(es[j].ts) })
	out := make([]M, 0, len(es))
	for _, e := range es {
		out = append(out, e.m)
	}
	return out
}

// Fecha+hora de un movimiento de inventario (hora de Ecuador).
func movTS(m M) time.Time {
	h := strOr(m["hora"], "00:00:00")
	t, err := time.ParseInLocation("2006-01-02T15:04:05", str(m["fecha"])+"T"+h, loc)
	if err != nil {
		return time.Time{}
	}
	return t
}

func entradaInv(m M) entrada {
	f := str(m["fecha"]) + "T" + strOr(m["hora"], "00:00:00")
	tipo := "salida"
	if str(m["tipo"]) == "entrada" {
		tipo = "entrada"
	}
	return entrada{movTS(m), M{
		"tipo": tipo, "producto": strOr(m["nombre"], strOr(m["motivo"], "Producto")), "cantidad": num(m["cantidad"]),
		"fecha": f, "horaRegistro": strOr(m["hora"], ""), "nota": str(m["motivo"]), "codigo": str(m["codigo"]),
	}}
}

// Desglosa los movimientos guardados dentro del documento de caja.
func desglosarCaja(movs []M) (transf float64, gastos, transfList []M, es []entrada) {
	gastos, transfList = []M{}, []M{}
	for _, m := range movs {
		ts, _ := parseTime(m["fecha"])
		monto := num(m["monto"])
		switch str(m["tipo"]) {
		case "transferencia":
			transf += monto
			transfList = append(transfList, m)
			nota := ""
			if c := str(m["comprobante"]); c != "" {
				nota = "Ref: " + c
			}
			es = append(es, entrada{ts, entradaM("entrada", "Transferencia — "+str(m["banco"]), monto, m["fecha"], nota)})
		case "gasto":
			gastos = append(gastos, m)
			es = append(es, entrada{ts, entradaM("salida", strOr(m["motivo"], "Gasto de caja"), monto, m["fecha"], "Egreso")})
		case "ingreso":
			es = append(es, entrada{ts, entradaM("entrada", strOr(m["motivo"], "Ingreso"), monto, m["fecha"], "Ingreso efectivo")})
		}
	}
	return
}

func cajaAbrir(w http.ResponseWriter, r *http.Request) {
	b := readBody(w, r)
	monto := num(b["monto"])
	if monto <= 0 {
		send(w, M{"error": "Capital inicial de apertura no válido"})
		return
	}
	abiertas, err := db.Collection("cajas").Where("activa", "==", true).Documents(ctx).GetAll()
	if err != nil {
		fail(w, 500, "Error crítico al perturbar estado de caja")
		return
	}
	if len(abiertas) > 0 {
		_, _ = abiertas[0].Ref.Update(ctx, []firestore.Update{{Path: "activa", Value: false}, {Path: "horaCierre", Value: nowISO()}})
	}
	ahora := nowISO()
	_, _, err = db.Collection("cajas").Add(ctx, M{
		"apertura": monto, "ingresos": 0, "gastos": 0, "activa": true, "horaApertura": ahora,
		"movimientos": []any{M{"tipo": "inicio", "monto": monto, "motivo": "Apertura operativa de caja", "fecha": ahora}},
	})
	if err != nil {
		fail(w, 500, "Error crítico al perturbar estado de caja")
		return
	}
	send(w, M{"ok": true})
}

func getCaja(w http.ResponseWriter, r *http.Request) {
	vacio := M{"apertura": 0, "ingresos": 0, "gastos": 0, "transferencias": 0, "saldo": 0}
	snap, err := cajaActiva()
	if err != nil || snap == nil {
		if err != nil {
			log.Println("❌ Error al leer caja activa:", err)
		}
		send(w, vacio)
		return
	}
	caja := snap.Data()
	transf, gastos, transfList, es := desglosarCaja(asList(caja["movimientos"]))

	apertura, _ := parseTime(caja["horaApertura"])
	ahora := time.Now()

	inv, err := db.Collection("movimientos-inventario").OrderBy("fecha", firestore.Desc).Limit(500).Documents(ctx).GetAll()
	if err != nil {
		log.Println("❌ Error al leer caja activa:", err)
		send(w, vacio)
		return
	}
	for _, d := range inv {
		m := d.Data()
		if strings.HasPrefix(str(m["motivo"]), "[CREDITO]") {
			continue
		}
		if ts := movTS(m); !ts.Before(apertura) && !ts.After(ahora) {
			es = append(es, entradaInv(m))
		}
	}

	send(w, M{
		"apertura": caja["apertura"], "ingresos": caja["ingresos"], "transferencias": transf, "gastos": caja["gastos"],
		"saldo":        num(caja["apertura"]) + num(caja["ingresos"]) - num(caja["gastos"]),
		"horaApertura": caja["horaApertura"], "gastosLista": gastos, "transferenciasList": transfList,
		"movimientos": ordenar(es),
	})
}

func cajaGasto(w http.ResponseWriter, r *http.Request) {
	b := readBody(w, r)
	caja, err := cajaActiva()
	if err != nil {
		fail(w, 500, "Error de red al consolidar débito")
		return
	}
	if caja == nil {
		send(w, M{"error": "Ninguna terminal de caja se encuentra activa"})
		return
	}
	monto := num(b["monto"])
	if monto <= 0 {
		send(w, M{"error": "Importe inválido"})
		return
	}
	mov := M{"tipo": "gasto", "monto": monto, "motivo": strOr(b["motivo"], "Gasto misceláneo de caja"), "fecha": nowISO()}
	if err := cajaRegistrar(caja, 0, monto, mov); err != nil {
		fail(w, 500, "Error de red al consolidar débito")
		return
	}
	send(w, M{"ok": true})
}

func cajaIngreso(w http.ResponseWriter, r *http.Request) {
	b := readBody(w, r)
	caja, err := cajaActiva()
	if err != nil {
		fail(w, 500, "Error al registrar ingreso de mercadería")
		return
	}
	if caja == nil {
		send(w, M{"error": "Ninguna terminal de caja se encuentra activa"})
		return
	}
	monto := num(b["monto"])
	if monto <= 0 {
		send(w, M{"error": "Importe inválido"})
		return
	}
	motivo := strOr(b["motivo"], strOr(b["producto"], "Entrada de mercadería"))
	mov := M{
		"tipo": "ingreso", "monto": monto, "motivo": motivo, "producto": strOr(b["producto"], motivo),
		"cantidad": monto, "nota": str(b["nota"]),
		"horaRegistro": strOr(b["horaRegistro"], time.Now().In(loc).Format("15:04:05")),
		"fecha":        nowISO(),
	}
	if err := cajaRegistrar(caja, monto, 0, mov); err != nil {
		fail(w, 500, "Error al registrar ingreso de mercadería")
		return
	}
	err = registrarMovimiento("entrada", strOr(b["codigo"], "-"), strOr(b["producto"], motivo), monto,
		strOr(b["nota"], "Entrada registrada desde panel de flujo"))
	if err != nil {
		fail(w, 500, "Error al registrar ingreso de mercadería")
		return
	}
	send(w, M{"ok": true})
}

func cajaTransferencia(w http.ResponseWriter, r *http.Request) {
	b := readBody(w, r)
	caja, err := cajaActiva()
	if err != nil {
		fail(w, 500, "Fallo de comunicación en asiento bancario")
		return
	}
	if caja == nil {
		send(w, M{"error": "La caja se encuentra cerrada"})
		return
	}
	monto := num(b["monto"])
	if monto <= 0 {
		send(w, M{"error": "Importe bancario fuera de rango"})
		return
	}
	mov := M{
		"tipo": "transferencia", "monto": monto, "motivo": "Ingreso directo por transferencia - " + str(b["banco"]),
		"banco": str(b["banco"]), "cuenta": str(b["cuenta"]), "comprobante": str(b["comprobante"]),
		"remitente": str(b["remitente"]), "fecha": nowISO(),
	}
	if err := cajaRegistrar(caja, monto, 0, mov); err != nil {
		fail(w, 500, "Fallo de comunicación en asiento bancario")
		return
	}
	send(w, M{"ok": true})
}

func cajaCerrar(w http.ResponseWriter, r *http.Request) {
	b := readBody(w, r)
	snap, err := cajaActiva()
	if err != nil {
		fail(w, 500, "Fallo general en protocolo de arqueo")
		return
	}
	if snap == nil {
		send(w, M{"error": "No hay actividades vigentes en caja"})
		return
	}
	caja := snap.Data()
	real, dejar := num(b["montoReal"]), num(b["dejar"])
	esperado := num(caja["apertura"]) + num(caja["ingresos"]) - num(caja["gastos"])
	diferencia := real - esperado

	var transf float64
	gastosLista, transfList := []M{}, []M{}
	for _, m := range asList(caja["movimientos"]) {
		switch str(m["tipo"]) {
		case "gasto":
			gastosLista = append(gastosLista, m)
		case "transferencia":
			transfList = append(transfList, m)
			transf += num(m["monto"])
		}
	}

	ahora := nowISO()
	movs, _ := caja["movimientos"].([]any)
	movs = append(movs, M{"tipo": "cierre", "monto": real, "motivo": "Cierre contable de jornada | Fondo retenido: $" + fmtNum(dejar), "fecha": ahora})
	caja["activa"], caja["cierre"], caja["horaCierre"], caja["dejado"], caja["movimientos"] = false, real, ahora, dejar, movs
	if err := updateMap(snap.Ref, caja); err != nil {
		fail(w, 500, "Fallo general en protocolo de arqueo")
		return
	}

	if dejar > 0 {
		_, _, err := db.Collection("cajas").Add(ctx, M{
			"apertura": dejar, "ingresos": 0, "gastos": 0, "activa": true, "horaApertura": ahora,
			"movimientos": []any{M{"tipo": "inicio", "monto": dejar, "motivo": "Fondo de apertura automático poscierre", "fecha": ahora}},
		})
		if err != nil {
			fail(w, 500, "Fallo general en protocolo de arqueo")
			return
		}
	}

	send(w, M{
		"apertura": caja["apertura"], "ingresos": caja["ingresos"], "transferencias": transf, "gastos": caja["gastos"],
		"esperado": esperado, "real": real, "diferencia": diferencia, "dejar": dejar,
		"fechaApertura": caja["horaApertura"], "fechaCierre": ahora,
		"gastosLista": gastosLista, "transferenciasList": transfList, "movimientos": movs,
	})
}

func cajaHistorial(w http.ResponseWriter, r *http.Request) {
	// Sin orderBy en Firestore: no hace falta índice compuesto. Se ordena en memoria.
	cs, err := db.Collection("cajas").Where("activa", "==", false).Documents(ctx).GetAll()
	if err != nil {
		log.Println("❌ Error en historial de cajas:", err)
		fail(w, 500, "Fallo de lectura en base histórica")
		return
	}
	cerradas := mapDocs(cs)
	sort.SliceStable(cerradas, func(i, j int) bool {
		a, _ := parseTime(cerradas[i]["horaCierre"])
		b, _ := parseTime(cerradas[j]["horaCierre"])
		return a.After(b)
	})
	if len(cerradas) > 50 {
		cerradas = cerradas[:50]
	}

	invDocs, err := db.Collection("movimientos-inventario").OrderBy("fecha", firestore.Desc).Documents(ctx).GetAll()
	if err != nil {
		log.Println("❌ Error en historial de cajas:", err)
		fail(w, 500, "Fallo de lectura en base histórica")
		return
	}
	inv := make([]M, 0, len(invDocs))
	for _, d := range invDocs {
		inv = append(inv, d.Data())
	}

	historial := make([]M, 0, len(cerradas))
	for _, c := range cerradas {
		transf, gastos, transfList, es := desglosarCaja(asList(c["movimientos"]))
		apertura, _ := parseTime(c["horaApertura"])
		cierre, _ := parseTime(c["horaCierre"])
		for _, m := range inv {
			if ts := movTS(m); !ts.Before(apertura) && !ts.After(cierre) {
				es = append(es, entradaInv(m))
			}
		}
		esperado := num(c["apertura"]) + num(c["ingresos"]) - num(c["gastos"])
		historial = append(historial, M{
			"fechaApertura": c["horaApertura"], "fechaCierre": c["horaCierre"], "apertura": c["apertura"],
			"ingresos": c["ingresos"], "transferencias": transf, "gastos": c["gastos"], "real": c["cierre"],
			"esperado": esperado, "diferencia": num(c["cierre"]) - esperado, "dejar": num(c["dejado"]),
			"gastosLista": gastos, "transferenciasList": transfList, "movimientos": ordenar(es),
		})
	}
	send(w, historial)
}

// =========================================================================
// ANÁLISIS
// =========================================================================

type prodStat struct {
	Nombre   string  `json:"nombre"`
	Vendidos float64 `json:"vendidos"`
	Ganancia float64 `json:"ganancia"`
}

func top5(lista []prodStat, less func(a, b prodStat) bool) []prodStat {
	c := append([]prodStat{}, lista...)
	sort.SliceStable(c, func(i, j int) bool { return less(c[i], c[j]) })
	if len(c) > 5 {
		c = c[:5]
	}
	return c
}

func getAnalisis(w http.ResponseWriter, r *http.Request) {
	docs, err := db.Collection("ventas").Documents(ctx).GetAll()
	if err != nil {
		log.Println("ERROR REAL EN GET /analisis:", err)
		fail(w, 500, "Fallo al procesar las métricas de ventas")
		return
	}
	ventas := mapDocs(docs)

	var totalGeneral float64
	var efectivo, credito, transferencia int
	porDia, porMes := M{}, M{}
	idx := map[string]int{}
	lista := []prodStat{}
	clientes := map[string]bool{}

	for _, v := range ventas {
		total := num(v["total"])
		totalGeneral += total
		switch str(v["tipo"]) {
		case "efectivo":
			efectivo++
		case "credito":
			credito++
		case "transferencia":
			transferencia++
		}

		diaS, mes := "Desconocido", "Desconocido"
		if f := aFechaISO(v["fecha"]); len(f) >= 10 {
			diaS = f[:10]
			if len(f) >= 7 {
				mes = f[:7]
			}
		}
		porDia[diaS] = num(porDia[diaS]) + total
		porMes[mes] = num(porMes[mes]) + total
		clientes[strOr(v["cedula"], str(v["cliente"]))] = true

		for _, p := range asList(v["productos"]) {
			nombre := strOr(p["nombre"], "Sin nombre")
			var cant float64
			switch {
			case p["amount"] != nil:
				cant = num(p["amount"])
			case p["cantidad"] != nil:
				cant = num(p["cantidad"])
			default:
				cant = 1
			}
			costo := num(p["precioCosto"])
			if costo == 0 {
				costo = num(p["costo"])
			}
			ganancia := (num(p["precio"]) - costo) * cant

			i, ok := idx[nombre]
			if !ok {
				i = len(lista)
				idx[nombre] = i
				lista = append(lista, prodStat{Nombre: nombre})
			}
			lista[i].Vendidos += cant
			lista[i].Ganancia += ganancia
		}
	}

	send(w, M{
		"ventas": ventas, "totalGeneral": totalGeneral, "efectivo": efectivo, "credito": credito,
		"transferencia": transferencia, "clientes": len(clientes), "porDia": porDia, "porMes": porMes,
		"masVendidos":   top5(lista, func(a, b prodStat) bool { return a.Vendidos > b.Vendidos }),
		"menosVendidos": top5(lista, func(a, b prodStat) bool { return a.Vendidos < b.Vendidos }),
		"masGanancia":   top5(lista, func(a, b prodStat) bool { return a.Ganancia > b.Ganancia }),
		"menosGanancia": top5(lista, func(a, b prodStat) bool { return a.Ganancia < b.Ganancia }),
	})
}

// ═════════════ cheques_archivos.go ═════════════
// =========================================================================
// CHEQUES POR PAGAR
// =========================================================================

func getCheques(w http.ResponseWriter, r *http.Request) {
	docs, err := db.Collection("cheques").OrderBy("fecha", firestore.Asc).Documents(ctx).GetAll()
	if err != nil {
		log.Println("❌ Error al leer cheques:", err)
		writeJSON(w, 500, []M{})
		return
	}
	send(w, mapDocs(docs))
}

func postCheque(w http.ResponseWriter, r *http.Request) {
	b := readBody(w, r)
	nuevo := M{
		"beneficiario":  strOr(b["beneficiario"], "Sin especificar"),
		"monto":         num(b["monto"]),
		"fecha":         strOr(b["fecha"], hoyISOServidor()),
		"numero":        str(b["numero"]),
		"notas":         str(b["notas"]),
		"pagado":        false,
		"fechaPago":     nil,
		"actualizadoEn": time.Now().UnixMilli(),
	}
	ref, _, err := db.Collection("cheques").Add(ctx, nuevo)
	if err != nil {
		log.Println("❌ Error al crear cheque:", err)
		fail(w, 500, "No se pudo registrar el cheque")
		return
	}
	nuevo["_id"] = ref.ID
	send(w, M{"ok": true, "cheque": nuevo})
}

func putCheque(w http.ResponseWriter, r *http.Request) {
	b := readBody(w, r)
	up := M{"actualizadoEn": time.Now().UnixMilli()}
	for _, k := range []string{"beneficiario", "fecha", "numero", "notas"} {
		if v, ok := b[k]; ok {
			up[k] = v
		}
	}
	if v, ok := b["monto"]; ok {
		up["monto"] = num(v)
	}
	if err := updateMap(db.Collection("cheques").Doc(r.PathValue("id")), up); err != nil {
		log.Println("❌ Error al editar cheque:", err)
		fail(w, 500, "No se pudo editar el cheque")
		return
	}
	send(w, M{"ok": true})
}

func pagarCheque(w http.ResponseWriter, r *http.Request) {
	b := readBody(w, r)
	ref := db.Collection("cheques").Doc(r.PathValue("id"))
	_, existe, err := getDoc(ref)
	if err != nil {
		fail(w, 500, "No se pudo marcar el cheque como pagado")
		return
	}
	if !existe {
		fail(w, 404, "Cheque no encontrado")
		return
	}
	fechaPago := strOr(b["fechaPago"], hoyISOServidor())
	if err := updateMap(ref, M{"pagado": true, "fechaPago": fechaPago, "actualizadoEn": time.Now().UnixMilli()}); err != nil {
		log.Println("❌ Error al marcar cheque como pagado:", err)
		fail(w, 500, "No se pudo marcar el cheque como pagado")
		return
	}
	send(w, M{"ok": true, "fechaPago": fechaPago})
}

func despagarCheque(w http.ResponseWriter, r *http.Request) {
	err := updateMap(db.Collection("cheques").Doc(r.PathValue("id")),
		M{"pagado": false, "fechaPago": nil, "actualizadoEn": time.Now().UnixMilli()})
	if err != nil {
		fail(w, 500, "No se pudo revertir el pago del cheque")
		return
	}
	send(w, M{"ok": true})
}

func deleteCheque(w http.ResponseWriter, r *http.Request) {
	if _, err := db.Collection("cheques").Doc(r.PathValue("id")).Delete(ctx); err != nil {
		fail(w, 500, "No se pudo eliminar el cheque")
		return
	}
	send(w, M{"ok": true})
}

func getChequesNotas(w http.ResponseWriter, r *http.Request) {
	docs, err := db.Collection("notas-cheques").Documents(ctx).GetAll()
	if err != nil {
		log.Println("❌ Error al leer notas de cheques:", err)
		writeJSON(w, 500, M{})
		return
	}
	notas := M{}
	for _, d := range docs {
		notas[d.Ref.ID] = str(d.Data()["texto"])
	}
	send(w, notas)
}

func putChequesNotas(w http.ResponseWriter, r *http.Request) {
	b := readBody(w, r)
	if _, err := db.Collection("notas-cheques").Doc(r.PathValue("mes")).Set(ctx, M{"texto": str(b["texto"])}); err != nil {
		fail(w, 500, "No se pudo guardar la nota del mes")
		return
	}
	send(w, M{"ok": true})
}

// =========================================================================
// ARCHIVOS — GESTOR TIPO GOOGLE DRIVE
// =========================================================================

func getArchivos(w http.ResponseWriter, r *http.Request) {
	var carpetaID any // nil = raíz
	if c := r.URL.Query().Get("carpeta"); c != "" {
		carpetaID = c
	}
	cs, err1 := db.Collection("carpetas").Where("parentId", "==", carpetaID).Documents(ctx).GetAll()
	as, err2 := db.Collection("archivos").Where("carpetaId", "==", carpetaID).Documents(ctx).GetAll()
	if err1 != nil || err2 != nil {
		log.Println("❌ Error al listar archivos:", err1, err2)
		fail(w, 500, "No se pudo leer el contenido de la carpeta")
		return
	}
	carpetas, archivos := mapDocs(cs), mapDocs(as)
	sort.SliceStable(carpetas, func(i, j int) bool { return str(carpetas[i]["nombre"]) < str(carpetas[j]["nombre"]) })
	sort.SliceStable(archivos, func(i, j int) bool { return num(archivos[i]["creadoEn"]) > num(archivos[j]["creadoEn"]) })
	send(w, M{"carpetas": carpetas, "archivos": archivos})
}

func getRutaArchivos(w http.ResponseWriter, r *http.Request) {
	ruta := []M{}
	actual := r.PathValue("id")
	for vueltas := 0; actual != "" && actual != "null" && vueltas < 30; vueltas++ {
		snap, existe, err := getDoc(db.Collection("carpetas").Doc(actual))
		if err != nil {
			log.Println("❌ Error al armar la ruta de carpetas:", err)
			writeJSON(w, 500, []M{})
			return
		}
		if !existe {
			break
		}
		d := snap.Data()
		ruta = append([]M{{"_id": snap.Ref.ID, "nombre": d["nombre"]}}, ruta...)
		actual = str(d["parentId"])
	}
	send(w, ruta)
}

func postCarpeta(w http.ResponseWriter, r *http.Request) {
	b := readBody(w, r)
	nombre := strings.TrimSpace(str(b["nombre"]))
	if nombre == "" {
		fail(w, 400, "El nombre de la carpeta es obligatorio")
		return
	}
	nueva := M{"nombre": nombre, "parentId": nilIfEmpty(b["parentId"]), "creadoEn": time.Now().UnixMilli()}
	ref, _, err := db.Collection("carpetas").Add(ctx, nueva)
	if err != nil {
		log.Println("❌ Error al crear carpeta:", err)
		fail(w, 500, "No se pudo crear la carpeta")
		return
	}
	nueva["_id"] = ref.ID
	send(w, M{"ok": true, "carpeta": nueva})
}

func subirArchivo(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 100<<20+1<<20)
	file, hdr, err := r.FormFile("archivo")
	if err != nil {
		fail(w, 400, "No se recibió ningún archivo")
		return
	}
	defer file.Close()
	if cld == nil {
		fail(w, 503, "Cloudinary no está configurado en el servidor (faltan variables CLOUDINARY_*)")
		return
	}

	var carpetaID any
	if c := r.FormValue("carpetaId"); c != "" && c != "null" {
		carpetaID = c
	}
	nombre := hdr.Filename
	ts := time.Now().UnixMilli()
	carpetaCld := "raiz"
	if c, ok := carpetaID.(string); ok {
		carpetaCld = c
	}

	data, err := io.ReadAll(file)
	if err != nil {
		fail(w, 400, "No se pudo leer el archivo")
		return
	}
	pid := strings.TrimSuffix(fmt.Sprintf("%d-%s", ts, nombre), filepath.Ext(nombre))
	res, err := cld.Upload.Upload(ctx, bytes.NewReader(data), uploader.UploadParams{
		Folder:       "archivos/" + carpetaCld,
		PublicID:     pid,
		ResourceType: "auto",
	})
	if err != nil {
		log.Println("❌ Error al subir archivo:", err)
		failDet(w, 500, "No se pudo subir el archivo", err)
		return
	}

	tipo := hdr.Header.Get("Content-Type")
	if tipo == "" {
		tipo = "application/octet-stream"
	}
	reg := M{
		"nombre": nombre, "carpetaId": carpetaID, "publicId": res.PublicID, "resourceType": res.ResourceType,
		"url": res.SecureURL, "tipo": tipo, "tamano": hdr.Size, "creadoEn": ts,
	}
	ref, _, err := db.Collection("archivos").Add(ctx, reg)
	if err != nil {
		failDet(w, 500, "No se pudo subir el archivo", err)
		return
	}
	reg["_id"] = ref.ID
	send(w, M{"ok": true, "archivo": reg})
}

func postEnlace(w http.ResponseWriter, r *http.Request) {
	b := readBody(w, r)
	nombre, url := strings.TrimSpace(str(b["nombre"])), strings.TrimSpace(str(b["url"]))
	if nombre == "" {
		fail(w, 400, "El nombre del enlace es obligatorio")
		return
	}
	low := strings.ToLower(url)
	if !strings.HasPrefix(low, "http://") && !strings.HasPrefix(low, "https://") {
		fail(w, 400, "El enlace debe ser una URL válida (http:// o https://)")
		return
	}
	reg := M{
		"nombre": nombre, "carpetaId": nilIfEmpty(b["carpetaId"]), "publicId": nil, "resourceType": "enlace",
		"url": url, "tipo": "enlace", "tamano": 0, "creadoEn": time.Now().UnixMilli(),
	}
	ref, _, err := db.Collection("archivos").Add(ctx, reg)
	if err != nil {
		log.Println("❌ Error al agregar enlace:", err)
		fail(w, 500, "No se pudo agregar el enlace")
		return
	}
	reg["_id"] = ref.ID
	send(w, M{"ok": true, "archivo": reg})
}

func borrarEnCloudinary(data M) error {
	pid := str(data["publicId"])
	if pid == "" || cld == nil {
		return nil
	}
	_, err := cld.Upload.Destroy(ctx, uploader.DestroyParams{
		PublicID:     pid,
		ResourceType: strOr(data["resourceType"], "image"),
	})
	return err
}

func deleteArchivo(w http.ResponseWriter, r *http.Request) {
	ref := db.Collection("archivos").Doc(r.PathValue("id"))
	snap, existe, err := getDoc(ref)
	if err != nil {
		fail(w, 500, "No se pudo eliminar el archivo")
		return
	}
	if !existe {
		fail(w, 404, "Archivo no encontrado")
		return
	}
	if e := borrarEnCloudinary(snap.Data()); e != nil {
		log.Println("⚠️ No se pudo borrar el objeto en Cloudinary:", e)
	}
	if _, err := ref.Delete(ctx); err != nil {
		fail(w, 500, "No se pudo eliminar el archivo")
		return
	}
	send(w, M{"ok": true})
}

func borrarCarpetaRecursiva(id string) error {
	subs, err := db.Collection("carpetas").Where("parentId", "==", id).Documents(ctx).GetAll()
	if err != nil {
		return err
	}
	archs, err := db.Collection("archivos").Where("carpetaId", "==", id).Documents(ctx).GetAll()
	if err != nil {
		return err
	}
	for _, d := range archs {
		_ = borrarEnCloudinary(d.Data())
		if _, err := d.Ref.Delete(ctx); err != nil {
			return err
		}
	}
	for _, d := range subs {
		if err := borrarCarpetaRecursiva(d.Ref.ID); err != nil {
			return err
		}
		if _, err := d.Ref.Delete(ctx); err != nil {
			return err
		}
	}
	return nil
}

func deleteCarpeta(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := borrarCarpetaRecursiva(id); err != nil {
		log.Println("❌ Error al eliminar carpeta:", err)
		fail(w, 500, "No se pudo eliminar la carpeta")
		return
	}
	if _, err := db.Collection("carpetas").Doc(id).Delete(ctx); err != nil {
		fail(w, 500, "No se pudo eliminar la carpeta")
		return
	}
	send(w, M{"ok": true})
}
