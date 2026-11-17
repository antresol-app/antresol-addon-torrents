package migrate

import mediaimport "github.com/antresol-app/antresol-addon-torrents/internal/corecopy/mediaimport/runkit"

// What the migration shares with the core media-server import
// (internal/mediaimport): the report and its lines, and the paths of the
// old services. Every importer here writes lines of that Report.
type (
	Line    = mediaimport.Line
	Report  = mediaimport.Report
	PathMap = mediaimport.PathMap
)

// Line statuses.
const (
	StatusPlanned  = mediaimport.StatusPlanned
	StatusImported = mediaimport.StatusImported
	StatusSkipped  = mediaimport.StatusSkipped
	StatusFailed   = mediaimport.StatusFailed
	StatusWarning  = mediaimport.StatusWarning
)

var (
	NewReportID = mediaimport.NewReportID
	SaveReport  = mediaimport.SaveReport
	LoadReport  = mediaimport.LoadReport
	ListReports = mediaimport.ListReports
)
