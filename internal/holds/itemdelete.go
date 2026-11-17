package holds

import "github.com/antresol-app/antresol/addonkit"

// Deleting a library item in the core takes along the data of a download
// whose hold names the item (ItemID: the item the download was made for,
// the only link before the import's phase 2 binds the library files' refs)
// or whose key is the ref of a deleted file, once everything the library
// made from it goes: the hold's selected files go to the trash, which
// stops the torrent with its data kept (files.plan / files.stop); a
// download that holds files of other library items too keeps running and
// the sheet shows keptWarningText. Stream torrents are left to the stream
// cache (no ItemID). Busy (being put into the library now) makes the core
// refuse the delete.

// keptWarningText is the note of a delete that leaves a torrent running:
// it holds files of other library items too ({name}: the core puts the
// download's name in).
func keptWarningText(name string) addonkit.Text {
	return text("Раздача «%s» продолжится: в ней есть и другое из медиатеки", name)
}
