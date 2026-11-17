// The seeding block of «Управление → Статистика» (statsBlocks, admins):
// how many downloads are shared now and what was uploaded to others in
// all, with its ratio to what was downloaded once it is worth saying.
// Nothing while it loads or when the addon does not answer.
import { formatSize, locale, ui } from '@antresol/addon-sdk'
import { useEffect, useState } from 'react'
import { getSeedingStats, type SeedingStats as Stats } from '../api'
import { useT } from '../i18n'
import styles from './SeedingStats.module.css'

const { Card } = ui

/** «2,3»: one decimal in the interface language. */
const decimal = (n: number) => new Intl.NumberFormat(locale(), { minimumFractionDigits: 1, maximumFractionDigits: 1 }).format(n)

export function SeedingStats() {
  const t = useT()
  const [data, setData] = useState<Stats | null>(null)
  useEffect(() => {
    const ac = new AbortController()
    getSeedingStats(ac.signal).then(
      (s) => !ac.signal.aborted && setData(s),
      () => {}, // the page goes on without the block
    )
    return () => ac.abort()
  }, [])
  if (!data) return null
  return (
    <div className={styles.tiles} data-testid="seeding-stats">
      <Card className={styles.tile}>
        <span className={styles.tileValue}>{data.count}</span>
        <span className={styles.tileLabel}>{t('stats.tile.seeding', { count: data.count })}</span>
      </Card>
      <Card className={styles.tile}>
        <span className={styles.tileValue}>{formatSize(data.uploadedTotal)}</span>
        <span className={styles.tileLabel}>
          {t('stats.uploaded')}
          {data.ratio >= 1.05 ? t('stats.ratio', { n: decimal(data.ratio) }) : ''}
        </span>
      </Card>
    </div>
  )
}
