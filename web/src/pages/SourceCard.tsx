// A torrent source in «Источники»: name, language, status in plain words
// («Подключено ✓» or «Ошибка: …»), an on/off switch and a ⋯ menu
// (change, check again, remove). A backup way in (RuTracker by cookie for
// RuTracker by login) is one more line of the same card. A source brought
// in from Prowlarr's list says so («из Prowlarr · 2 ч назад»).
import { locale, Icon, ui } from '@antresol/addon-sdk'
import { ago } from '../format'
import { type Source } from '../types'
import { tr, useT } from '../i18n'
import styles from "./SourceCard.module.css";

const { Badge, IconButton, MenuItem, MenuSeparator, Menu, Toggle } = ui

export interface SourceCardProps {
  source: Source;
  /** The definition's language (ru-RU …), when known. */
  language?: string;
  checking?: boolean;
  onToggle: (on: boolean) => void;
  onTest: () => void;
  onRemove: () => void;
  /** «Изменить…»: the source's form (login, password, cookie). */
  onEdit?: () => void;
  /** The backup way into this source, asked only when it fails. */
  backup?: Source;
  backupChecking?: boolean;
  onBackupTest?: () => void;
  onBackupEdit?: () => void;
}

export function languageLabel(lang?: string): string {
  if (!lang) return "";
  const l = lang.toLowerCase();
  if (l.startsWith("ru")) return tr("common.lang.ru");
  if (l.startsWith("en")) return tr("common.lang.en");
  if (l.startsWith("uk")) return tr("common.lang.uk");
  try {
    return (
      new Intl.DisplayNames([locale()], { type: "language" }).of(
        lang.split("-")[0]!,
      ) ?? lang
    );
  } catch {
    return lang;
  }
}

export function SourceCard({
  source: s,
  language,
  checking,
  onToggle,
  onTest,
  onRemove,
  onEdit,
  backup,
  backupChecking,
  onBackupTest,
  onBackupEdit,
}: SourceCardProps) {
  const t = useT();
  const lang = languageLabel(language);
  return (
    <li
      className={`${styles.card} ${s.enabled ? "" : styles.off}`}
      data-testid="source-card"
    >
      <div className={styles.top}>
        <span className={styles.logo} aria-hidden>
          {s.name.trim().charAt(0).toLocaleUpperCase(locale())}
        </span>
        <div className={styles.text}>
          <p className={styles.name}>{s.name}</p>
          {lang && <p className={styles.lang}>{lang}</p>}
          {s.origin === "prowlarr" && (
            <p className={styles.origin}>
              <Badge>
                {s.syncedAt
                  ? t("comp.source.prowlarrSynced", { ago: ago(s.syncedAt) })
                  : t("comp.source.prowlarr")}
              </Badge>
            </p>
          )}
        </div>
        <Toggle
          label={t("comp.source.toggle", {
            name: s.name,
            state: t(s.enabled ? "comp.source.on" : "comp.source.off"),
          })}
          checked={s.enabled}
          onChange={onToggle}
        />
      </div>
      <p className={styles.status} role="status">
        {checking ? (
          <span className={styles.checking}>
            <span className={styles.spinner} aria-hidden />{" "}
            {t("comp.source.checking")}
          </span>
        ) : !s.enabled ? (
          <span className={styles.muted}>{t("comp.source.disabled")}</span>
        ) : s.status === "ok" ? (
          <span className={styles.ok}>{t("comp.source.connected")}</span>
        ) : s.status === "error" ? (
          <>
            <span className={styles.error}>
              {t("comp.source.error", {
                error: s.lastError || t("comp.source.noResponse"),
              })}
            </span>
            {onEdit && (
              <button type="button" className={styles.fix} onClick={onEdit}>
                {t("comp.source.fixSignIn")}
              </button>
            )}
          </>
        ) : (
          <span className={styles.muted}>{t("comp.source.notChecked")}</span>
        )}
      </p>
      {backup && s.enabled && (
        <p className={styles.backup}>
          {s.status === "error" && backup.status === "ok" && !checking && (
            <span className={styles.muted}>
              {t("comp.source.backupInUse")}{" "}
            </span>
          )}
          <span className={styles.muted}>
            {t("comp.source.backup", {
              name: "cookie" in backup.settings ? "cookie" : backup.name,
            })}
          </span>
          {backupChecking ? (
            <span className={styles.checking}>
              <span className={styles.spinner} aria-hidden />{" "}
              {t("comp.source.checking")}
            </span>
          ) : backup.status === "ok" ? (
            <span className={styles.ok}>{t("comp.source.backupOk")}</span>
          ) : backup.status === "error" ? (
            <span className={styles.error}>
              {t("comp.source.backupError", {
                error: backup.lastError || t("comp.source.noResponse"),
              })}
            </span>
          ) : (
            <span className={styles.muted}>
              {t("comp.source.backupNotChecked")}
            </span>
          )}
        </p>
      )}
      <div className={styles.more}>
        <Menu
          label={t("comp.source.actions", { name: s.name })}
          placement="bottom-end"
          trigger={(p) => (
            <IconButton {...p} size="xs" variant="plain" icon="ellipsis" label={t("comp.source.more", { name: s.name })} />
          )}
        >
          {(close) => (
            <>
              {onEdit && (
                <MenuItem
                  icon={<Icon name="sliders" size={17} />}
                  label={t("comp.source.edit")}
                  onSelect={() => {
                    close(false);
                    onEdit();
                  }}
                />
              )}
              <MenuItem
                icon={<Icon name="restart" size={17} />}
                label={t("comp.source.recheck")}
                onSelect={() => {
                  close();
                  onTest();
                }}
              />
              {backup && onBackupEdit && (
                <MenuItem
                  icon={<Icon name="sliders" size={17} />}
                  label={t("comp.source.backupEdit")}
                  onSelect={() => {
                    close(false);
                    onBackupEdit();
                  }}
                />
              )}
              {backup && onBackupTest && (
                <MenuItem
                  icon={<Icon name="restart" size={17} />}
                  label={t("comp.source.backupRecheck")}
                  onSelect={() => {
                    close();
                    onBackupTest();
                  }}
                />
              )}
              <MenuSeparator />
              <MenuItem
                icon={<Icon name="trash" size={17} />}
                label={t("comp.source.remove")}
                onSelect={() => {
                  close(false);
                  onRemove();
                }}
              />
            </>
          )}
        </Menu>
      </div>
    </li>
  );
}
