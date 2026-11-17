// The addon's «Переезд» page (routes 'migrate', admins): the core's reusable
// page over the addon's own x/admin/migrate… and its progress event, with
// the addon's part (MigrateFull). The core's «Переезд» leads here through
// the addon's migrateSources card.
import { MigratePage, useEvents, type MigrateApi } from '@antresol/addon-sdk'
import { ID, migrateReport, migrateReports, migrateRun, migrateState, migrateStatus } from '../api'
import { migrateFull } from './MigrateFull'

/** The short name admin.migrate.progress as admins get it (contract «Events»). */
export const MIGRATE_PROGRESS = `addon.${ID}.admin.migrate.progress`
const EVENTS = [MIGRATE_PROGRESS]

/** One object for the page's life: the page calls its useProgress on every render. */
export const addonMigrateApi: MigrateApi = {
  migrateStatus,
  migrateState,
  migrateRun,
  migrateReports,
  migrateReport,
  useProgress: (handler) => useEvents((e) => handler(e.data), EVENTS),
}

export function MigratePageRoute() {
  // keyed: a page of another api is a new page, never a change of hooks
  return <MigratePage key={ID} api={addonMigrateApi} full={migrateFull} />
}
