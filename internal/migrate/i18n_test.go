package migrate

import (
	"context"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	_ "github.com/antresol-app/antresol-addon-torrents/internal/strings" // the English catalogs
)

func TestReportLocalize(t *testing.T) {
	ctx := i18n.WithLanguage(context.Background(), i18n.EN)
	rep := &Report{Lines: []Line{
		{Name: "Сериал (2020)", Reason: "Включится кнопкой «Перейти на Antresol» · отслеживается 1 серия"},
		{Name: "x", Reason: "Не удалось остановить раздачу в qBittorrent: boom — раздача возвращена в qBittorrent"},
		{Name: "x", Reason: keptNote(leftInQB("Данные на диске не совпадают с торрентом (93 %)"))},
		{Name: "x", Reason: "Не удалось проверить правила RSS qBittorrent — посмотрите там вручную, что не переносится"},
		{Name: "Sonarr", Reason: "Перенос из Sonarr прервался: что-то пошло не так (подробности в журнале Antresol)"},
	}}
	rep.Localize(ctx)
	want := []Line{
		{Name: "Сериал (2020)", Reason: "Turns on with “Switch to Antresol” · following 1 episode"},
		{Name: "x", Reason: "Couldn’t stop the torrent in qBittorrent: boom — the torrent was returned to qBittorrent"},
		{Name: "x", Reason: "The data on disk doesn’t match the torrent (93 %) — the torrent stayed in qBittorrent (it stayed paused in Antresol)"},
		{Name: "x", Reason: "Couldn’t check qBittorrent RSS rules — look there yourself for what isn’t moved"},
		{Name: "Sonarr", Reason: "Moving from Sonarr stopped: something went wrong (details in the Antresol log)"},
	}
	for i, l := range rep.Lines {
		if l != want[i] {
			t.Errorf("line %d = %+v, want %+v", i, l, want[i])
		}
	}
}
