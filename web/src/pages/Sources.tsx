// «Источники»: the configured torrent sites as cards with a plain status
// and an on/off switch; «Добавить источник» and «Импортировать из Prowlarr».
import { ApiError, useEvents, Icon, ui } from '@antresol/addon-sdk'
import { deleteSource, getCatalog, getProwlarrLink, getSources, importProwlarr, testSource, updateSource } from '../api'
import { type ImportedSource, type ProwlarrLink, type Source } from '../types'
import { SourceCard } from './SourceCard'
import { tr, useT } from '../i18n'
import { setTitle } from '../title'
import { useCallback, useEffect, useState } from "react";
import { Link, useNavigate } from "react-router";
import m from "./Manage.module.css";
import styles from "./Sources.module.css";

const { Button, ButtonLink, SegmentedControl, Sheet, TextField } = ui

export const DEFAULT_PROWLARR_DB = "/prowlarr/prowlarr.db";

type State =
  | { status: "loading" }
  | { status: "error"; message: string }
  | { status: "ready"; list: Source[] };

/** Language of each definition, for the cards (the list has no fields; fetched once). */
let languages: Promise<Map<string, string>> | null = null;
function useLanguages() {
  const [map, setMap] = useState<Map<string, string>>(new Map());
  useEffect(() => {
    languages ??= getCatalog("").then(
      (all) => new Map(all.map((e) => [e.id, e.language])),
      () => {
        languages = null;
        return new Map();
      },
    );
    let live = true;
    void languages.then((m) => live && setMap(m));
    return () => {
      live = false;
    };
  }, []);
  return map;
}

/** How Prowlarr's tracker list is reached now; null until known (or when the addon cannot say). */
function useProwlarrLink() {
  const [link, setLink] = useState<ProwlarrLink | null>(null);
  useEffect(() => {
    const ac = new AbortController();
    getProwlarrLink(ac.signal).then(
      (r) => setLink(r.link),
      () => {},
    );
    return () => ac.abort();
  }, []);
  return link;
}

export function Sources() {
  const t = useT();
  const navigate = useNavigate();
  const [state, setState] = useState<State>({ status: "loading" });
  const [checking, setChecking] = useState<Set<number>>(new Set());
  const [problem, setProblem] = useState("");
  const [removing, setRemoving] = useState<Source | null>(null);
  const [importing, setImporting] = useState(false);
  const langs = useLanguages();
  const prowlarr = useProwlarrLink();

  const load = useCallback((quiet = false) => {
    if (!quiet) setState({ status: "loading" });
    getSources().then(
      (list) => setState({ status: "ready", list }),
      (e: unknown) =>
        !quiet &&
        setState({
          status: "error",
          message: e instanceof ApiError ? e.message : tr("common.loadFailed"),
        }),
    );
  }, []);
  useEffect(() => {
    setTitle(tr("sources.title"));
    load();
  }, [load]);
  useEvents(
    (e) => {
      const d = e.data as { id?: number; status?: string; lastError?: string };
      if (typeof d?.id !== "number") return;
      setState((s) =>
        s.status === "ready"
          ? {
              ...s,
              list: s.list.map((x) =>
                x.id === d.id
                  ? {
                      ...x,
                      status: d.status ?? x.status,
                      lastError: d.lastError ?? "",
                    }
                  : x,
              ),
            }
          : s,
      );
    },
    ["addon.torrents.admin.source-status"],
  );

  const replace = (src: Source) =>
    setState((s) =>
      s.status === "ready"
        ? { ...s, list: s.list.map((x) => (x.id === src.id ? src : x)) }
        : s,
    );

  const check = useCallback(async (id: number) => {
    setChecking((c) => new Set(c).add(id));
    try {
      replace(await testSource(id));
    } catch (e) {
      setProblem(
        e instanceof ApiError ? e.message : tr("sources.list.checkFailed"),
      );
    } finally {
      setChecking((c) => {
        const n = new Set(c);
        n.delete(id);
        return n;
      });
    }
  }, []);

  const toggle = async (s: Source, on: boolean) => {
    setProblem("");
    replace({ ...s, enabled: on });
    try {
      replace(await updateSource(s.id, { enabled: on }));
      if (on) void check(s.id);
    } catch (e) {
      replace(s);
      setProblem(e instanceof ApiError ? e.message : t("sources.failedRetry"));
    }
  };

  // a backup way in (RuTracker by cookie) is part of its source's card
  const all = state.status === "ready" ? state.list : [];
  const list = all.filter((s) => !s.fallbackFor);
  const backupOf = new Map(
    all.filter((s) => s.fallbackFor).map((s) => [s.fallbackFor!, s]),
  );
  const broken = list.filter((s) => s.enabled && s.status === "error").length;

  return (
    <section aria-labelledby="src-title">
      <header className={m.header}>
        <div>
          <h2 id="src-title" className={m.title}>
            {t("sources.title")}
          </h2>
          <p className={m.subtitle}>
            {state.status === "ready" && list.length > 0
              ? t("sources.list.enabledOf", {
                  on: list.filter((s) => s.enabled).length,
                  total: list.length,
                }) + (broken ? t("sources.list.withErrors", { n: broken }) : "")
              : t("sources.list.subtitle")}
          </p>
        </div>
        <div className={m.headerActions}>
          <Button
            variant="secondary"
            icon="arrowDown"
            onClick={() => setImporting(true)}
          >
            {t("sources.list.importProwlarr")}
          </Button>
          <ButtonLink to="/addons/torrents/sources/add" icon="plus">
            {t("sources.add.title")}
          </ButtonLink>
        </div>
      </header>
      {prowlarr && list.some((s) => s.origin === "prowlarr") && (
        <p className={styles.prowlarrLink} data-testid="prowlarr-link">
          {t(`sources.prowlarrLink.${prowlarr}`)}{" "}
          <Link to="/addons/torrents/migrate">{t("sources.prowlarrLink.manage")}</Link>
        </p>
      )}
      {problem && (
        <div className={m.problem} role="alert">
          {problem}
        </div>
      )}
      {state.status === "loading" && (
        <div className={m.spinnerRow} role="status">
          <span className={m.spinner} aria-hidden /> {t("sources.loading")}
        </div>
      )}
      {state.status === "error" && (
        <div className={m.problem} role="alert">
          {state.message}
        </div>
      )}
      {state.status === "ready" && list.length === 0 && (
        <div className={m.empty}>
          <Icon name="plug" size={40} strokeWidth={1.4} />
          <h3>{t("sources.list.emptyTitle")}</h3>
          <p>{t("sources.list.emptyText")}</p>
          <ButtonLink to="/addons/torrents/sources/add" icon="plus">
            {t("sources.add.title")}
          </ButtonLink>
        </div>
      )}
      {list.length > 0 && (
        <ul className={styles.grid}>
          {list.map((s) => (
            <SourceCard
              key={s.id}
              source={s}
              language={langs.get(s.definitionId)}
              checking={checking.has(s.id)}
              onToggle={(on) => void toggle(s, on)}
              onTest={() => void check(s.id)}
              onRemove={() => setRemoving(s)}
              onEdit={() => navigate(`/addons/torrents/sources/${s.id}/edit`)}
              backup={backupOf.get(s.id)}
              backupChecking={
                backupOf.has(s.id) && checking.has(backupOf.get(s.id)!.id)
              }
              onBackupTest={() => {
                const b = backupOf.get(s.id);
                if (b) void check(b.id);
              }}
              onBackupEdit={() => {
                const b = backupOf.get(s.id);
                if (b) navigate(`/addons/torrents/sources/${b.id}/edit`);
              }}
            />
          ))}
        </ul>
      )}
      {removing && (
        <Sheet
          title={t("sources.list.removeTitle", { name: removing.name })}
          message={t("sources.list.removeMessage")}
          icon={<Icon name="trash" size={34} strokeWidth={1.6} />}
          onCancel={() => setRemoving(null)}
          actions={[
            {
              label: t("common.cancel"),
              kind: "cancel",
              run: () => setRemoving(null),
            },
            {
              label: t("common.delete"),
              kind: "destructive",
              run: () => {
                const s = removing;
                setRemoving(null);
                deleteSource(s.id).then(
                  () =>
                    setState((st) =>
                      st.status === "ready"
                        ? {
                            ...st,
                            list: st.list.filter(
                              (x) => x.id !== s.id && x.fallbackFor !== s.id,
                            ),
                          }
                        : st,
                    ),
                  (e: unknown) =>
                    setProblem(
                      e instanceof ApiError
                        ? e.message
                        : tr("sources.list.removeFailed"),
                    ),
                );
              },
            },
          ]}
        />
      )}
      {importing && (
        <ImportSheet
          apiKept={prowlarr === "api"}
          onClose={() => setImporting(false)}
          onImported={(added) => {
            load(true);
            for (const s of added) void check(s.id);
          }}
        />
      )}
    </section>
  );
}

function ImportSheet({
  apiKept,
  onClose,
  onImported,
}: {
  /** «Переезд» keeps a connection to Prowlarr's API: it is offered first. */
  apiKept: boolean;
  onClose: () => void;
  onImported: (added: Source[]) => void;
}) {
  const t = useT();
  const [path, setPath] = useState(DEFAULT_PROWLARR_DB);
  const [via, setVia] = useState<"api" | "file">(apiKept ? "api" : "file");
  // the link may come after the sheet opened
  useEffect(() => {
    if (apiKept) setVia("api");
  }, [apiKept]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [result, setResult] = useState<ImportedSource[] | null>(null);

  const run = async () => {
    setBusy(true);
    setError("");
    try {
      const r = await importProwlarr(via === "api" ? { via } : { via, path: path.trim() });
      setResult(r.imported);
      onImported(r.imported.flatMap((x) => (x.source ? [x.source] : [])));
    } catch (e) {
      setError(
        e instanceof ApiError ? e.message : t("sources.prowlarr.failed"),
      );
    } finally {
      setBusy(false);
    }
  };

  // a source with a reason was there already (its cookie was updated): it is
  // re-checked with the added ones but listed with its reason
  const added = result?.filter((x) => x.source && !x.reason) ?? [];
  const skipped = result?.filter((x) => !x.source || x.reason) ?? [];
  return (
    <Sheet
      title={t("sources.prowlarr.title")}
      message={result ? undefined : t("sources.prowlarr.message")}
      icon={<Icon name="arrowDown" size={30} strokeWidth={1.8} />}
      role="dialog"
      size="wide"
      busy={busy}
      onCancel={onClose}
      actions={
        result
          ? []
          : [
              { label: t("common.cancel"), kind: "cancel", run: onClose },
              {
                label: t("sources.prowlarr.import"),
                kind: "primary",
                disabled: via === "file" && !path.trim(),
                run: () => void run(),
              },
            ]
      }
    >
      {!result && apiKept && (
        <div className={styles.importField}>
          <SegmentedControl
            label={t("sources.prowlarr.via")}
            value={via}
            onChange={setVia}
            segments={[
              { value: "api", label: t("sources.prowlarr.viaApi") },
              { value: "file", label: t("sources.prowlarr.viaFile") },
            ]}
          />
          {via === "api" && (
            <p className={styles.importHint}>{t("sources.prowlarr.apiHint")}</p>
          )}
          {via === "api" && error && (
            <p className={m.problem} role="alert">
              {error}
            </p>
          )}
        </div>
      )}
      {!result && via === "file" && (
        <div className={styles.importField}>
          <TextField
            label={t("sources.prowlarr.fileLabel")}
            value={path}
            onChange={(e) => setPath(e.target.value)}
            hint={t("sources.prowlarr.fileHint")}
            error={error || null}
            spellCheck={false}
          />
        </div>
      )}
      {result && (
        <div className={styles.importResult}>
          <p className={m.ok}>
            {added.length > 0
              ? t("sources.prowlarr.added", { n: added.length })
              : t("sources.prowlarr.none")}
          </p>
          {added.length > 0 && (
            <ul>
              {added.map((x) => (
                <li key={x.name}>
                  <Icon name="check" size={14} strokeWidth={2.6} /> {x.name}
                  {x.hidden?.length ? ` — ${t("sources.prowlarr.hidden", { fields: x.hidden.join(", ") })}` : ""}
                </li>
              ))}
            </ul>
          )}
          {skipped.length > 0 && (
            <>
              <p className={styles.skippedTitle}>
                {t("sources.prowlarr.skipped")}
              </p>
              <ul>
                {skipped.map((x) => (
                  <li key={x.name}>
                    {x.name}
                    {x.reason ? ` — ${x.reason}` : ""}
                  </li>
                ))}
              </ul>
            </>
          )}
        </div>
      )}
    </Sheet>
  );
}
