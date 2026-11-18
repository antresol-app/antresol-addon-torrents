// «Переезд» from Prowlarr, Sonarr, Radarr and qBittorrent: the addon's part
// of its own instance of the core's page (MigratePage, MigratePageRoute):
// these services' cards, switching on the follows taken over from
// Sonarr/Radarr (after confirming the number of downloads and that they are
// stopped), moving the torrents again and giving them back to qBittorrent.
import { useState } from 'react'
import { ApiError, Icon, ui, type MigrateCtx, type MigrateFullApi, type SourceMeta } from '@antresol/addon-sdk'
import { migrateRollback, migrateSwitch, migrateSwitchPreview } from '../api'
import { tr, useT, type Key } from '../i18n'
import type { MigrateReport, MigrateSwitchPlan } from '../types'
import m from '../pages/Manage.module.css'
import full from './MigrateFull.module.css'
import { SourceSetup } from './SourceSetup'

const { Button, Sheet } = ui

/** The product's name in these texts (the core's brand). */
const APP_NAME = 'Antresol'

/** The server's words for a failed call, or a plain «Не получилось». */
const errorText = (e: unknown) => (e instanceof ApiError ? e.message : tr('migrate.failedRetry'))

/** An addon text for the page: drawn in the interface language when the page draws it. */
const text = (key: Key) => () => tr(key, { app: APP_NAME })

/** The services this addon moves. */
const SOURCES: Record<string, SourceMeta> = {
  prowlarr: { icon: 'plug', what: text('migrate.what.prowlarr') },
  sonarr: { icon: 'tv', what: text('migrate.what.sonarr') },
  radarr: { icon: 'film', what: text('migrate.what.radarr') },
  qbittorrent: { icon: 'download', what: text('migrate.what.qbittorrent'), note: text('migrate.qbNote') },
}

/** «Перейти на Vault» when follows wait; «Повторить перенос qBittorrent» once switched. */
function TopCards({ ctx }: { ctx: MigrateCtx }) {
  const t = useT()
  const { status, running, ready, busy, setBusy, setProblem, reload, check } = ctx
  const [switchPlan, setSwitchPlan] = useState<MigrateSwitchPlan | null>(null)
  const [arrStopped, setArrStopped] = useState(false)
  const [switchError, setSwitchError] = useState('')
  const [switchDone, setSwitchDone] = useState(false)

  const pendingFollows = status.pendingFollows ?? 0
  const switchedAt = status.state?.switchedAt
  const qbFound = status.sources.some((s) => s.id === 'qbittorrent' && s.found)

  const openSwitch = async () => {
    setSwitchError('')
    setArrStopped(false)
    setSwitchDone(false)
    setBusy('switch')
    try {
      setSwitchPlan(await migrateSwitchPreview())
    } catch (e) {
      setProblem(errorText(e))
    } finally {
      setBusy('')
    }
  }
  const doSwitch = async () => {
    if (!switchPlan) return
    setBusy('switch')
    setSwitchError('')
    try {
      await migrateSwitch({ confirm: switchPlan.episodes + switchPlan.movies, arrStopped })
      setSwitchDone(true)
      reload()
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) {
        // the count changed: show the new one
        migrateSwitchPreview().then(setSwitchPlan, () => {})
      }
      setSwitchError(errorText(e))
    } finally {
      setBusy('')
    }
  }

  return (
    <>
      {pendingFollows > 0 && (
        <div className={`${full.switchCard} glass`} data-testid="migrate-switch-card">
          <div className={full.switchIcon} aria-hidden>
            <Icon name="arrowRight" size={22} strokeWidth={2.2} />
          </div>
          <div className={full.switchText}>
            <h3>{t('migrate.switch', { app: APP_NAME })}</h3>
            <p>{t('migrate.switch.waiting', { count: pendingFollows, app: APP_NAME })}</p>
          </div>
          <Button icon="arrowRight" loading={busy === 'switch' && !switchPlan} disabled={busy !== '' || !!running} onClick={() => void openSwitch()}>
            {t('migrate.switch', { app: APP_NAME })}
          </Button>
        </div>
      )}
      {switchedAt && pendingFollows === 0 && qbFound && (
        <div className={`${full.switchCard} glass`}>
          <div className={`${full.switchIcon} ${full.switchIconDone}`} aria-hidden>
            <Icon name="check" size={22} strokeWidth={2.4} />
          </div>
          <div className={full.switchText}>
            <h3>{t('migrate.qbAgain')}</h3>
            <p>{t('migrate.qbAgain.text', { app: APP_NAME })}</p>
          </div>
          <Button variant="secondary" disabled={!ready || !!running || busy !== ''} onClick={() => check(['qbittorrent'])}>
            {t('migrate.qbCheck')}
          </Button>
        </div>
      )}

      {switchPlan && (
        <Sheet
          title={switchDone ? t('common.done') : t('migrate.switch', { app: APP_NAME })}
          wide
          role="alertdialog"
          icon={<Icon name={switchDone ? 'checkCircle' : 'arrowRight'} size={34} strokeWidth={1.6} />}
          busy={busy === 'switch'}
          message={
            switchDone ? (
              <p>{t('migrate.switch.done')}</p>
            ) : switchPlan.blocked ? (
              <>
                <p data-testid="migrate-switch-text">{switchPlan.blocked}.</p>
                <p className={full.sheetNote}>{t('migrate.blocked.why', { app: APP_NAME })}</p>
                <p className={full.sheetNote}>{t('migrate.blocked.what', { app: APP_NAME })}</p>
              </>
            ) : (
              <>
                <p data-testid="migrate-switch-text">
                  {t('migrate.switch.question', {
                    follows: t('migrate.switch.follows', { count: switchPlan.follows }),
                    episodes: t('migrate.episodes', { count: switchPlan.episodes }),
                    movies: t('migrate.movies', { count: switchPlan.movies }),
                  })}
                </p>
                {switchPlan.errors > 0 && (
                  <p className={full.sheetNote}>{t('migrate.switch.tmdbErrors', { count: switchPlan.errors, app: APP_NAME })}</p>
                )}
                <p className={full.sheetNote}>{t('migrate.switch.stopArr')}</p>
                <label className={full.confirmCheck}>
                  <input type="checkbox" checked={arrStopped} onChange={(e) => setArrStopped(e.target.checked)} />
                  <span>{t('migrate.switch.arrStopped')}</span>
                </label>
                {switchError && (
                  <p className={full.sheetError} role="alert">
                    {switchError}
                  </p>
                )}
              </>
            )
          }
          onCancel={() => setSwitchPlan(null)}
          actions={
            switchDone || switchPlan.blocked
              ? [{ label: t('common.close'), kind: 'primary', run: () => setSwitchPlan(null) }]
              : [
                  { label: t('common.cancel'), kind: 'cancel', run: () => setSwitchPlan(null) },
                  { label: t('migrate.switch.turnOn'), kind: 'primary', disabled: !arrStopped, run: () => void doSwitch() },
                ]
          }
        />
      )}
    </>
  )
}

/** «Вернуть раздачи в qBittorrent» on a real run that moved torrents (or «Раздачи возвращены»). */
function HistoryAction({
  ctx,
  report,
  rolledBack,
  onStarted,
}: {
  ctx: MigrateCtx
  report: MigrateReport
  rolledBack: boolean
  onStarted: (reportId: string) => void
}) {
  const t = useT()
  const { running, busy, setBusy, setProblem } = ctx
  const [confirm, setConfirm] = useState(false)
  if (report.dryRun || report.rollbackOf || (report.torrents ?? 0) === 0 || report.id === running) return null
  if (rolledBack) return <span className={m.rowHint}>{t('migrate.returned')}</span>

  const doRollback = async () => {
    setBusy('rollback')
    try {
      const { reportId } = await migrateRollback(report.id)
      setConfirm(false)
      onStarted(reportId)
    } catch (e) {
      setConfirm(false)
      setBusy('')
      setProblem(errorText(e))
    }
  }

  return (
    <>
      <Button size="small" variant="secondary" disabled={!!running || busy !== ''} onClick={() => setConfirm(true)}>
        {t('migrate.rollback')}
      </Button>
      {confirm && (
        <Sheet
          title={t('migrate.rollback.title')}
          icon={<Icon name="restart" size={34} strokeWidth={1.6} />}
          busy={busy === 'rollback'}
          message={t('migrate.rollback.message', { count: report.torrents ?? 0, app: APP_NAME })}
          onCancel={() => setConfirm(false)}
          actions={[
            { label: t('common.cancel'), kind: 'cancel', run: () => setConfirm(false) },
            { label: t('migrate.rollback.action'), kind: 'destructive', run: () => void doRollback() },
          ]}
        />
      )}
    </>
  )
}

/** The confirmation's items for this addon's services. */
function ConfirmItems({ sources }: { sources: string[] }) {
  const t = useT()
  return (
    <>
      {sources.includes('qbittorrent') && <li>{t('migrate.confirm.qb', { app: APP_NAME })}</li>}
      {(sources.includes('sonarr') || sources.includes('radarr')) && <li>{t('migrate.confirm.arr', { app: APP_NAME })}</li>}
      {sources.includes('prowlarr') && <li>{t('migrate.confirm.prowlarr')}</li>}
    </>
  )
}

export const migrateFull: MigrateFullApi = {
  source: (id) => SOURCES[id],
  TopCards,
  HistoryAction,
  ConfirmItems,
  progressNote: (current) => (current === 'qbittorrent' ? tr('migrate.progress.qb') : ''),
  subtitle: text('migrate.subtitleFull'),
  rollbackTitle: text('migrate.report.rollback'),
  SourceSetup,
}
