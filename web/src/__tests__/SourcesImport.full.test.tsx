import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import type { ImportedSource, Source } from "../types";
import { Sources } from "../pages/Sources";

const json = (b: unknown, status = 200) =>
  new Response(JSON.stringify(b), {
    status,
    headers: { "Content-Type": "application/json" },
  });

/** jsdom has no EventSource. */
class FakeSource {
  addEventListener() {}
  close() {}
}

const src = (id: number, name: string): Source => ({
  id,
  definitionId: "noname-clubl",
  name,
  enabled: true,
  settings: {},
  status: "unknown",
  priority: 25,
});

afterEach(() => vi.unstubAllGlobals());

describe("Prowlarr import sheet", () => {
  it("lists a source whose cookie was updated with its reason, not as added, and re-checks it", async () => {
    const imported: ImportedSource[] = [
      { name: "Kinozal", source: src(1, "Kinozal") },
      {
        name: "NoNaMe ClubL",
        source: src(2, "NoNaMe ClubL"),
        reason: "Уже добавлен: cookie обновлён из сеанса Prowlarr",
      },
      { name: "Lost", reason: "Описание трекера «lost» не найдено" },
    ];
    const tested: string[] = [];
    vi.stubGlobal("EventSource", FakeSource);
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        if (url.includes("/api/addons/torrents/x/admin/sources/import-prowlarr"))
          return json({ imported });
        const m = url.match(/\/x\/admin\/sources\/(\d+)\/test/);
        if (m && init?.method === "POST") {
          tested.push(m[1] ?? "");
          return json(src(Number(m[1]), "x"));
        }
        if (url.includes("/api/addons/torrents/x/admin/sources")) return json([]);
        if (url.includes("/api/catalog")) return json([]);
        return json({ error: "нет" }, 404);
      }),
    );
    render(
      <MemoryRouter>
        <Sources />
      </MemoryRouter>,
    );
    await userEvent.click(
      await screen.findByRole("button", { name: "Импортировать из Prowlarr" }),
    );
    const dialog = await screen.findByRole("dialog");
    await userEvent.click(
      within(dialog).getByRole("button", { name: "Импортировать" }),
    );
    expect(
      await within(dialog).findByText(/Добавлено: 1\./),
    ).toBeInTheDocument();
    expect(
      within(dialog).getByText(
        "NoNaMe ClubL — Уже добавлен: cookie обновлён из сеанса Prowlarr",
      ),
    ).toBeInTheDocument();
    expect(
      within(dialog).getByText("Lost — Описание трекера «lost» не найдено"),
    ).toBeInTheDocument();
    await waitFor(() => expect(tested.sort()).toEqual(["1", "2"]));
  });
});

describe("a backup way in", () => {
  it("shows RuTracker by cookie inside RuTracker by login: one card, its own status and actions", async () => {
    const list: Source[] = [
      {
        ...src(3, "RuTracker.org"),
        definitionId: "rutracker",
        status: "error",
        lastError: "Сайт не ответил",
      },
      {
        ...src(15, "RuTracker.org (cookie)"),
        definitionId: "rutracker-org-cookie",
        settings: { cookie: "••••••" },
        status: "ok",
        fallbackFor: 3,
      },
      { ...src(4, "Kinozal"), definitionId: "kinozal", status: "ok" },
    ];
    const tested: string[] = [];
    vi.stubGlobal("EventSource", FakeSource);
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        const m = url.match(/\/x\/admin\/sources\/(\d+)\/test/);
        if (m && init?.method === "POST") {
          tested.push(m[1] ?? "");
          return json(list.find((s) => s.id === Number(m[1])));
        }
        if (url.includes("/api/addons/torrents/x/admin/sources")) return json(list);
        if (url.includes("/api/catalog")) return json([]);
        return json({ error: "нет" }, 404);
      }),
    );
    render(
      <MemoryRouter>
        <Sources />
      </MemoryRouter>,
    );
    const cards = await screen.findAllByTestId("source-card");
    expect(cards).toHaveLength(2);
    const rt = cards.find((c) => within(c).queryByText("RuTracker.org"))!;
    expect(
      within(rt).getByText(/Запасной вход \(cookie\)/),
    ).toBeInTheDocument();
    expect(within(rt).getByText("работает ✓")).toBeInTheDocument();
    expect(
      within(rt).getByText(/поиск идёт через запасной/),
    ).toBeInTheDocument();
    await userEvent.click(
      within(rt).getByRole("button", { name: "Ещё: RuTracker.org" }),
    );
    await userEvent.click(
      await screen.findByRole("menuitem", { name: "Проверить запасной вход" }),
    );
    await waitFor(() => expect(tested).toContain("15"));
  });
});

describe("sources from Prowlarr", () => {
  it("marks a source from Prowlarr's list and says how Prowlarr is reached, with the way to «Переезд»", async () => {
    const synced = new Date(Date.now() - 2 * 3600_000).toISOString();
    vi.stubGlobal("EventSource", FakeSource);
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        if (url.includes("/x/admin/migrate/prowlarr")) return json({ link: "api" });
        if (url.includes("/api/addons/torrents/x/admin/sources"))
          return json([{ ...src(1, "Kinozal"), origin: "prowlarr", syncedAt: synced }, src(2, "Rutor")]);
        if (url.includes("/api/catalog")) return json([]);
        return json({ error: "нет" }, 404);
      }),
    );
    render(
      <MemoryRouter>
        <Sources />
      </MemoryRouter>,
    );
    const cards = await screen.findAllByTestId("source-card");
    expect(within(cards[0]!).getByText(/^из Prowlarr · /)).toBeInTheDocument();
    expect(within(cards[1]!).queryByText(/из Prowlarr/)).toBeNull();
    const line = await screen.findByTestId("prowlarr-link");
    expect(line).toHaveTextContent("Prowlarr подключён по API");
    expect(within(line).getByRole("link", { name: "Переезд" })).toHaveAttribute("href", "/addons/torrents/migrate");
  });
});
