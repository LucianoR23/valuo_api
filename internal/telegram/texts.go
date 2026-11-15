package telegram

import (
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/LucianoR23/valuo_api/internal/domain"
	"github.com/LucianoR23/valuo_api/internal/expenses"
	"github.com/LucianoR23/valuo_api/internal/money"
)

// Fixed replies (plan §8.4: the bot never shows model text, only these and
// what it builds from the person's own data).
const (
	example           = "«súper 15000 débito»"
	textLinked        = "Listo, tu cuenta quedó conectada. Mandame un gasto como " + example + " y te lo muestro antes de guardarlo.\n\nTambién: /resumen, /comprometido, /deshacer."
	textBadCode       = "Ese link venció o ya se usó. Pedí uno nuevo desde la app, en Ajustes › Telegram."
	textUnlinked      = "Desconectaste tu cuenta. Para volver a usar el bot, conectala desde la app, en Ajustes › Telegram."
	textHelp          = "Mandame un gasto como lo dirías: " + example + " o «nafta 30000 visa 3 cuotas ayer». Si es del Hogar, sumá «compartido». También podés mandar una nota de voz o la foto de un ticket o comprobante. Te lo muestro antes de guardarlo.\n\n/resumen: tu resumen de la semana\n/comprometido: lo que ya tenés comprometido en tarjetas\n/deshacer: borra el último gasto que cargaste acá"
	textNotFinance    = "Solo puedo registrar gastos. Probá: " + example + "."
	textNoAmount      = "No encontré el monto. Probá: " + example + "."
	textOnlyText      = "Puedo leer texto, notas de voz y fotos de tickets o comprobantes. Probá: " + example + "."
	textVoiceTooLong  = "Mandame notas de voz de hasta 30 segundos, o escribilo."
	textImageTooBig   = "La imagen es muy pesada. Mandala como foto, no como archivo, o escribí el gasto."
	textCantRead      = "No pude leer el monto. Mandame el gasto escrito, por ejemplo " + example + "."
	textAIPaused      = "La lectura automática está pausada en tu cuenta por ahora. Mandame el gasto con el monto, por ejemplo " + example + "."
	textNoMethods     = "Primero cargá un medio de pago en la app, así sé con qué pagaste."
	textReplaced      = "✏️ Corregido: mirá el mensaje de abajo."
	textAskCorrection = "✏️ Mandame la frase corregida."
	textGone          = "Ese gasto ya no está para confirmar."
	textAlreadySaved  = "Ya está guardado."
	textPickMethod    = "Elegí con qué pagaste."
	textSaveFailed    = "No pude guardarlo. Corregilo con ✏️ o cargalo en la app."
	textNothingToUndo = "No hay gastos cargados desde acá en las últimas 24 horas."
	textNoSummary     = "Todavía no tenés un resumen semanal."
	textError         = "Algo falló. Probá de nuevo en un rato."
)

func (s *Service) unlinkedText() string {
	return "Hola. Para cargar gastos acá, conectá tu cuenta de " + s.cfg.Brand + " desde la app, en Ajustes › Telegram."
}

func formatAmount(d draft) string {
	a, err := decimal.NewFromString(d.Amount)
	if err != nil {
		return d.Amount
	}
	return money.Format(a, d.Currency)
}

// summary1 is the draft in two lines:
//
//	Café · $ 2.500
//	Visa Galicia · 3 cuotas · hoy · Comida · Hogar
func (s *Service) summary1(d draft) string {
	parts := []string{}
	if d.Method != "" {
		parts = append(parts, d.Method)
	}
	if d.Installments > 1 {
		parts = append(parts, strconv.Itoa(d.Installments)+" cuotas")
	}
	parts = append(parts, s.day(d.SpentAt))
	if d.Category != "" {
		parts = append(parts, d.Category)
	}
	if d.Shared {
		parts = append(parts, "Hogar")
	}
	return d.Description + " · " + formatAmount(d) + "\n" + strings.Join(parts, " · ")
}

// similarLine is the draft's duplicate warning under the summary.
func similarLine(d draft) string {
	if d.Similar == "" {
		return ""
	}
	return "\n\n" + d.Similar
}

// similarText describes a possible duplicate (plan 3.9):
//
//	⚠️ Parecido a uno que ya está: Carrefour · $ 12.500 · ayer (Ana)
//	⚠️ Tenés pendiente Luz del 10/11. Si es esa factura, completala desde la app.
func (s *Service) similarText(userID uuid.UUID, dup expenses.Duplicate) string {
	day := s.day(dup.SpentAt.Format(time.DateOnly))
	if dup.Status == "draft" {
		return "⚠️ Tenés pendiente " + dup.Description + " del " + day + ". Si es esa factura, completala desde la app."
	}
	who := ""
	if dup.CreatedBy != userID && dup.CreatedByName != "" {
		who = " (" + dup.CreatedByName + ")"
	}
	return "⚠️ Parecido a uno que ya está: " + dup.Description + " · " + money.Format(dup.Amount, dup.Currency) + " · " + day + who
}

// day is "hoy", "ayer" or "12/11".
func (s *Service) day(date string) string {
	t, err := time.Parse(time.DateOnly, date)
	if err != nil {
		return date
	}
	today := s.today()
	switch {
	case t.Equal(today):
		return "hoy"
	case t.Equal(today.AddDate(0, 0, -1)):
		return "ayer"
	case t.Year() == today.Year():
		return t.Format("2/1")
	default:
		return t.Format("2/1/2006")
	}
}

var months = [...]string{"enero", "febrero", "marzo", "abril", "mayo", "junio", "julio",
	"agosto", "septiembre", "octubre", "noviembre", "diciembre"}

// monthName turns "2026-12" into "Diciembre 2026".
func monthName(ym string) string {
	t, err := time.Parse("2006-01", ym)
	if err != nil {
		return ym
	}
	m := months[t.Month()-1]
	return strings.ToUpper(m[:1]) + m[1:] + " " + strconv.Itoa(t.Year())
}

// committedText lists "Comprometido" month by month (plan 4.2).
func committedText(c domain.Committed) string {
	if len(c.Cards) == 0 {
		return "No tenés tarjetas de crédito cargadas."
	}
	lines := []string{}
	for _, m := range c.Months {
		amounts := []string{}
		for _, t := range m.Totals {
			if !t.Amount.IsZero() {
				amounts = append(amounts, money.Format(t.Amount, t.Currency))
			}
		}
		if len(amounts) > 0 {
			lines = append(lines, monthName(m.PeriodYM)+": "+strings.Join(amounts, " · "))
		}
	}
	if len(lines) == 0 {
		return "No tenés nada comprometido en tarjetas."
	}
	return "Comprometido en tarjetas:\n" + strings.Join(lines, "\n")
}

// Commands is the bot's menu, registered on start.
var Commands = []Command{
	{Command: "resumen", Description: "Tu resumen de la semana"},
	{Command: "comprometido", Description: "Lo que ya tenés comprometido en tarjetas"},
	{Command: "deshacer", Description: "Borra el último gasto que cargaste acá"},
	{Command: "ayuda", Description: "Cómo cargar un gasto"},
}
