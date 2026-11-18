package indexer

import (
	"context"
	"errors"
	"net"
	"strings"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/opds"
)

var errSourceTimeout = errors.New("source timeout")

// humanError turns a source error into a short Russian message for the
// sources screen (stored as lastError: a catalog key or its rendering);
// technical details stay in the logs.
func humanError(err error) string {
	if err == nil {
		return ""
	}
	return humanErr(err).Error()
}

// humanErrorIn is humanError in the language of ctx.
func humanErrorIn(ctx context.Context, err error) string {
	if err == nil {
		return ""
	}
	return i18n.Message(ctx, humanErr(err))
}

// humanErr is the human message of a source error as an i18n error.
func humanErr(err error) error {
	if msg := opds.ErrorMessage(err); msg != "" {
		return i18n.Errorf(msg) // the address guard of OPDS and direct-download sources
	}
	switch {
	case errors.Is(err, errSourceTimeout), errors.Is(err, context.DeadlineExceeded):
		return i18n.Errorf("Сайт не ответил вовремя. Возможно, он перегружен или заблокирован.")
	case errors.Is(err, context.Canceled):
		return i18n.Errorf("Поиск отменён.")
	case errors.Is(err, ErrNoDefinition):
		return i18n.Errorf("Описание этого трекера не найдено. Добавьте источник заново.")
	case errors.Is(err, cardigann.ErrCaptcha):
		return i18n.Errorf("Сайт просит ввести капчу. Войдите через cookie из браузера — выберите вариант источника с «cookie».")
	case errors.Is(err, cardigann.ErrLoginFailed):
		if rest, ok := detail(err, cardigann.ErrLoginFailed); ok {
			// the detail is Russian text of a cardigann key ("cookie
			// устарел или неверен"): Localize translates it
			return i18n.Errorf("Не удалось войти на сайт: %s.", errors.New(cardigann.RedactText(rest)))
		}
		return i18n.Errorf("Не удалось войти на сайт. Проверьте логин и пароль.")
	case errors.Is(err, cardigann.ErrNoSolver):
		return i18n.Errorf("Сайт закрыт защитой от ботов (Cloudflare). Нужен FlareSolverr — укажите его адрес в «Управление → Настройки загрузок».")
	case errors.Is(err, cardigann.ErrCloudflare):
		return i18n.Errorf("Сайт закрыт защитой от ботов (Cloudflare), и FlareSolverr её не прошёл. Проверьте его в «Управление → Настройки загрузок».")
	case errors.Is(err, cardigann.ErrUnavailable):
		return i18n.Errorf("Сайт недоступен. Проверьте, открывается ли он в браузере, или укажите зеркало.")
	case errors.Is(err, cardigann.ErrNotFile):
		return i18n.Errorf("Сайт вернул страницу вместо файла. Возможно, он изменился или просит войти.")
	case errors.Is(err, cardigann.ErrNotTorrent):
		return i18n.Errorf("Сайт вернул не торрент-файл. Возможно, нужно войти заново.")
	case errors.Is(err, cardigann.ErrBadResponse):
		return i18n.Errorf("Сайт ответил не так, как ожидалось. Возможно, он изменился или временно сломан.")
	}
	var ne net.Error
	if errors.As(err, &ne) {
		if ne.Timeout() {
			return i18n.Errorf("Сайт не ответил вовремя. Возможно, он перегружен или заблокирован.")
		}
		return i18n.Errorf("Сайт недоступен. Проверьте, открывается ли он в браузере, или укажите зеркало.")
	}
	return i18n.Errorf("Что-то пошло не так при обращении к сайту. Подробности — в журнале сервера.")
}

// detail is the Russian explanation after a sentinel ("не удалось войти
// на сайт: cookie устарел или неверен" → "cookie устарел или неверен"),
// without a final period; false when there is none.
func detail(err, sentinel error) (string, bool) {
	msg := err.Error()
	i := strings.Index(msg, sentinel.Error()+": ")
	if i < 0 {
		return "", false
	}
	rest := msg[i+len(sentinel.Error())+2:]
	if rest == "" || strings.Contains(rest, "cardigann") {
		return "", false
	}
	return strings.TrimSuffix(rest, "."), true
}

// sourceError is an error with a human message that unwraps to the cause
// and to the message as an i18n error (human), so i18n.Message renders it
// in the request's language ("<source name>: <message>").
type sourceError struct {
	msg   string
	err   error
	human error
}

func (e *sourceError) Error() string { return e.msg }
func (e *sourceError) Unwrap() []error {
	var out []error
	for _, err := range []error{e.human, e.err} {
		if err != nil {
			out = append(out, err)
		}
	}
	return out
}

// named is a sourceError "<name>: <human>" ("<human>" without a name).
func named(name string, human, cause error) error {
	msg := human.Error()
	if name != "" {
		msg = name + ": " + msg
	}
	return &sourceError{msg: msg, err: cause, human: human}
}

func humanize(name string, err error) error {
	return named(name, humanErr(err), err)
}
