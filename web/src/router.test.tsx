import { StrictMode } from "react";
import { page, userEvent } from "vitest/browser";
import { describe, expect, it } from "vitest";
import { render } from "vitest-browser-react";
import { RouterProvider, createMemoryHistory } from "@tanstack/react-router";
import { createFactoryClient, type FactoryClient } from "@looprig/client";
import { browserFactoryComposition, createAppRouter, factoryBaseUrl } from "./router";
import { FactoryLinkProbe } from "./test/fakes";

const SID = "44444444-4444-4444-4444-444444444444";

interface Composed {
  probe: FactoryLinkProbe;
  router: ReturnType<typeof createAppRouter>;
}

/**
 * A whole application, composed the way `main.tsx` composes it, over doubles.
 *
 * ONE injection is load-bearing now, not two. The Factory link is a real
 * hazard: the provider opens a Centrifuge socket from an effect, so without a
 * fake link every test in this file would open a real WebSocket. The live-frame
 * source that used to be injected alongside it is gone — the detail route is
 * `FactorySessionDetailRoute`, which builds none — and passing a double into an
 * option nothing reads asserts nothing. `fetchProbe` below keeps the property
 * that injection was protecting, against the real global rather than a stub.
 *
 * `transport` is still constructed because the legacy `FakeTransport` is what
 * the row fixtures are written against; `compose` projects its list into the
 * probe's Factory reads. It is not passed to the router.
 */
function compose(path: string, sessions: { session_id: string; agent_id: string; state: string; last_active_at: string; title?: string }[] = [], probe = new FactoryLinkProbe()): Composed {
  probe.recentSessionsResult = Promise.resolve({ sessions });
  const router = createAppRouter({
    history: createMemoryHistory({ initialEntries: [path] }),
    factory: { options: probe.options() },
  });
  return { probe, router };
}

/**
 * Every URL the page's own `fetch` is asked for while the probe is installed.
 *
 * The property under test is negative — "the Factory detail route opens no Host
 * event stream" — and a negative is only worth asserting against the thing that
 * would actually carry it. The previous version of this assertion counted opens
 * on an INJECTED live source; once the route stopped taking one, that count was
 * zero because nothing could ever raise it. This watches `window.fetch`, which
 * a route reaching for `createFetchLiveFrameSource` would have to go through.
 */
function fetchProbe(): { urls: string[]; restore: () => void } {
  const urls: string[] = [];
  const original = window.fetch;
  window.fetch = (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    urls.push(typeof input === "string" ? input : input instanceof URL ? input.href : input.url);
    return original.call(window, input, init);
  };
  return { urls, restore: () => { window.fetch = original; } };
}

function at(path: string): ReturnType<typeof createAppRouter> {
  return compose(path).router;
}

function oneRow(): Composed {
  return compose("/sessions", [{ session_id: SID, agent_id: "agent-1", state: "idle", last_active_at: "2026-09-05T12:00:00Z", title: "Fix the parser" }]);
}

describe("router", () => {
  it("renders the sessions list at /sessions", async () => {
    render(<RouterProvider router={at("/sessions")} />);
    await expect.element(page.getByTestId("sessions-empty")).toBeInTheDocument();
  });

  it("passes the path's sid through to the session detail route", async () => {
    render(<RouterProvider router={at(`/sessions/${SID}`)} />);
    const id = page.getByTestId("detail-session-id");
    await expect.element(id).toBeInTheDocument();
    expect(id.element().textContent).toBe(SID);
  });

  it("sends the root path to the sessions list", async () => {
    const router = at("/");
    render(<RouterProvider router={router} />);
    await expect.element(page.getByTestId("sessions-empty")).toBeInTheDocument();
    expect(router.state.location.pathname).toBe("/sessions");
  });

  it("opens a session from the list without leaving the SPA", async () => {
    // The row is a real <a href>, so without client-side navigation this would
    // be a full document load: new bundle, lost state, and a flash. The router
    // hands SessionsPage an onOpenSession that intercepts the plain click.
    const router = oneRow().router;
    render(<RouterProvider router={router} />);
    await expect.element(page.getByTestId("session-row-link")).toBeInTheDocument();

    await userEvent.click(page.getByTestId("session-row-link"));
    await expect.element(page.getByTestId("detail-session-id")).toBeInTheDocument();
    expect(router.state.location.pathname).toBe(`/sessions/${SID}`);
  });

  it("routes on the path, not on the fragment", async () => {
    // Decision D2: browser history, departing from capstan's hash history.
    // wui.Assets() already serves the SPA fallback in Go, so /sessions/<uuid>
    // is a real, refreshable, linkable path -- hash history exists for hosts
    // that cannot do that.
    //
    // The discriminator: a hash-history router derives its whole location from
    // window.location.hash, so planting a path there is enough to tell the two
    // apart. Nothing navigates -- setting the fragment does not reload -- and
    // the fragment is put back before the assertion runs.
    const original = window.location.hash;
    window.history.replaceState(null, "", "#/planted/by/the/test");
    let observed: { pathname: string; href: string };
    try {
      const router = createAppRouter({
        factory: { options: new FactoryLinkProbe().options() },
      });
      observed = {
        pathname: router.history.location.pathname,
        href: router.history.location.href,
      };
    } finally {
      window.history.replaceState(null, "", window.location.pathname + window.location.search + original);
    }

    expect(observed.pathname).not.toBe("/planted/by/the/test");
    expect(observed.pathname).toBe(window.location.pathname);
    expect(observed.href.startsWith("#")).toBe(false);
  });
});

describe("Factory detail composition", () => {
  it("binds the detail route through the shared Factory link without a Host event source", async () => {
    const composed = compose(`/sessions/${SID}`);
    const fetches = fetchProbe();
    try {
      render(<RouterProvider router={composed.router} />);
      await expect.element(page.getByTestId("detail-session-id")).toBeInTheDocument();
      await expect.poll(() => composed.probe.only().open.length).toBe(1);
      expect(composed.probe.only().open[0]?.sessionId).toBe(SID);
      // The whole Host event plane, not just this session's: a route that
      // reached for one would name `/events` in the URL it asked `fetch` for.
      expect(fetches.urls.filter((url) => url.includes("/events"))).toStrictEqual([]);
    } finally {
      fetches.restore();
    }
  });

  it("releases the session binding when navigating back to the list", async () => {
    const composed = oneRow();
    const screen = await render(<RouterProvider router={composed.router} />);
    await expect.element(page.getByTestId("session-row-link")).toBeInTheDocument();

    await userEvent.click(page.getByTestId("session-row-link"));
    await expect.element(page.getByTestId("detail-session-id")).toBeInTheDocument();
    await expect.poll(() => composed.probe.only().open.length).toBe(1);

    // DEPARTURE closes the session binding. A binding left open on the way back
    // to the list is a live subscription per session ever visited, for as long
    // as the tab is open, so the probe's live `open` count is read after the
    // navigation rather than only after it was raised.
    await composed.router.navigate({ to: "/sessions" });
    await expect.element(page.getByTestId("session-row-link")).toBeInTheDocument();
    await expect.poll(() => composed.probe.only().open.length).toBe(0);
    expect(composed.probe.only().subscriptions[0]?.unsubscribeCount).toBe(1);

    // Unmounting the application closes the FACTORY socket by the same
    // argument, one layer up. `open` is the probe's live count — `maxOpen` is a
    // high-water mark and cannot fall — so this is the only assertion that can
    // see a provider whose cleanup never ran.
    await screen.unmount();
    await expect.poll(() => composed.probe.open).toBe(0);
    expect(composed.probe.only().state).toBe("disconnected");
  });
});

describe("one Factory client per application", () => {
  it("builds exactly one link for the whole app, across a navigation", async () => {
    const composed = oneRow();
    render(<RouterProvider router={composed.router} />);
    await expect.element(page.getByTestId("session-row-link")).toBeInTheDocument();

    await userEvent.click(page.getByTestId("session-row-link"));
    await expect.element(page.getByTestId("detail-session-id")).toBeInTheDocument();

    expect(composed.probe.links.length).toBe(1);
    await expect.poll(() => composed.probe.maxOpen).toBe(1);
    // Bootstrap and bounded durable reads are the only network work: opening
    // the detail sends no placement or other session command.
    expect(composed.probe.fetchCalls.map((call) => call.input)).toEqual([
      "/v1/bootstrap",
      "/v1/sessions?limit=100",
      `/v1/sessions/${SID}/status`,
      `/v1/sessions/${SID}/gates?limit=256`,
      `/v1/sessions/${SID}/journal?limit=256&tail=256`,
      `/v1/sessions/${SID}/status`,
      `/v1/sessions/${SID}/gates?limit=256`,
      `/v1/sessions/${SID}/journal?limit=256&tail=256`,
    ]);
    expect(composed.probe.only().rpcCalls).toEqual([]);
  });

  it("builds exactly one link under StrictMode's double mount", async () => {
    // The provider constructs its client in a ref, not a useMemo, precisely for
    // this: React double-invokes a useMemo factory and keeps one result, so a
    // memoised constructor allocates a second socket nothing will ever close.
    const composed = compose("/sessions");
    render(
      <StrictMode>
        <RouterProvider router={composed.router} />
      </StrictMode>,
    );
    await expect.element(page.getByTestId("sessions-empty")).toBeInTheDocument();

    expect(composed.probe.links.length).toBe(1);
    // StrictMode runs the open effect, its cleanup and the effect again, so the
    // ONE link is connected twice in sequence — which is a reconnect, not a
    // second socket. `maxOpen` is the concurrency bound and stays at one.
    await expect.poll(() => composed.probe.maxOpen).toBe(1);
    expect(composed.probe.only().connectCalls).toBeGreaterThan(1);
    // And the provider's cleanup really ran: the second connect follows a
    // disconnect rather than stacking on top of the first.
    expect(composed.probe.only().disconnectCalls).toBeGreaterThan(0);
  });

  it("counts a second application, so the bound above is not vacuous", async () => {
    // The negative assertion is worth nothing unless the probe can see two.
    const probe = new FactoryLinkProbe();
    const first = compose("/sessions", [], probe);
    const second = compose("/sessions", [], probe);
    render(
      <>
        <RouterProvider router={first.router} />
        <RouterProvider router={second.router} />
      </>,
    );
    await expect.poll(() => probe.links.length).toBe(2);
    await expect.poll(() => probe.maxOpen).toBe(2);
  });

  it("records a Factory REST read, so the empty fetchCalls above is not vacuous", async () => {
    // The other half of the same antidote, for the OTHER negative assertion.
    // `expect(probe.fetchCalls).toEqual([])` cannot tell "the application issued
    // no Factory REST request" from "the probe's `fetch` was never installed and
    // the request went to the real one": deleting `fetch: this.fetch` from
    // `FactoryLinkProbe.options()` left all sixteen tests in this file green.
    //
    // What makes it non-vacuous is showing that the plane the application
    // composed issues ITS reads here. `create` is `FactoryLinkProvider`'s own
    // construction seam and this one delegates to the real `createFactoryClient`
    // — it captures the client, it does not replace it, so the options-to-client
    // mapping under test is production's.
    const probe = new FactoryLinkProbe();
    const clients: FactoryClient[] = [];
    const router = createAppRouter({
      history: createMemoryHistory({ initialEntries: ["/sessions"] }),
      factory: {
        options: probe.options(),
        create: (options) => {
          const client = createFactoryClient(options);
          clients.push(client);
          return client;
        },
      },
    });
    render(<RouterProvider router={router} />);
    await expect.element(page.getByTestId("sessions-empty")).toBeInTheDocument();

    expect(clients.length).toBe(1);
    // Never settles (the probe's `fetch` returns a promise that does not), so
    // it is deliberately not awaited; `fetchCalls` is the observation.
    void clients[0]!.reads.listAgents();
    await expect.poll(() => probe.fetchCalls.some((call) => call.input === "/v1/agents")).toBe(true);
    expect(probe.fetchCalls.map((call) => call.input)).toEqual([
      "/v1/bootstrap", "/v1/sessions?limit=100", "/v1/agents",
    ]);
  });
});

describe("authenticated browser bootstrap", () => {
  it("does not mount routes or construct a socket before tenant verification", async () => {
    const probe = new FactoryLinkProbe();
    probe.bootstrapResult = new Promise(() => {});
    const composed = compose("/sessions", [], probe);
    render(<RouterProvider router={composed.router} />);

    await expect.element(page.getByTestId("factory-bootstrap-loading")).toBeInTheDocument();
    await expect.element(page.getByTestId("sessions-page")).not.toBeInTheDocument();
    expect(probe.links).toEqual([]);
    expect(probe.fetchCalls.map((call) => call.input)).toEqual(["/v1/bootstrap"]);
    expect(probe.fetchCalls[0]?.init?.cache).toBe("no-store");
  });

  it("fails closed on an authoritative bootstrap denial", async () => {
    const probe = new FactoryLinkProbe();
    probe.bootstrapResult = Promise.resolve(new Response(JSON.stringify({
      error: { code: "not_authorized", message: "tenant access denied", retryable: false },
    }), { status: 403 }));
    const composed = compose("/sessions", [], probe);
    render(<RouterProvider router={composed.router} />);

    await expect.element(page.getByTestId("factory-bootstrap-error")).toHaveTextContent("tenant access denied");
    await expect.element(page.getByTestId("sessions-page")).not.toBeInTheDocument();
    expect(probe.links).toEqual([]);
  });
});

describe("Factory connection credentials", () => {
  it("re-mints the connection token on every connect, including a reconnect", async () => {
    let issued = 0;
    const probe = new FactoryLinkProbe();
    const router = createAppRouter({
      history: createMemoryHistory({ initialEntries: ["/sessions"] }),
      factory: {
        options: probe.options(),
        credentials: {
          connectionToken: (): Promise<string> => {
            issued += 1;
            return Promise.resolve(`token-${issued}`);
          },
        },
      },
    });
    render(<RouterProvider router={router} />);
    await expect.element(page.getByTestId("sessions-empty")).toBeInTheDocument();

    const link = probe.only();
    await expect.poll(() => link.connectTokens).toEqual(["token-1"]);

    // A reconnect. Centrifuge calls `getToken` on every connect attempt, and
    // the real link installs `getToken: () => credentials.connectionToken!()`,
    // so the application's token function is re-entered rather than the first
    // token being replayed. A composition that captured a token at startup
    // would leave this at ["token-1"] and every reconnect would present an
    // expired credential.
    link.disconnect();
    await link.connect();
    expect(link.connectTokens).toEqual(["token-1", "token-2"]);
  });

  it("installs no token hook for an application that supplies no credentials", async () => {
    // The other half of the pair: the link decides at CONSTRUCTION whether the
    // token hook exists, so a forwarder installed for a caller with no token
    // provider is a hook that can only fail. Without this, the test above is
    // satisfied by a composition that always installs one.
    const composed = compose("/sessions");
    render(<RouterProvider router={composed.router} />);
    await expect.element(page.getByTestId("sessions-empty")).toBeInTheDocument();

    expect(composed.probe.only().credentials.connectionToken).toBeUndefined();
    expect(composed.probe.only().connectTokens).toEqual([]);
  });
});

describe("Factory base URL", () => {
  it("derives the realtime endpoint from a custom base URL", async () => {
    const probe = new FactoryLinkProbe();
    const router = createAppRouter({
      history: createMemoryHistory({ initialEntries: ["/sessions"] }),
      factory: { options: probe.options({ baseUrl: "https://factory.example.test:9443" }) },
    });
    render(<RouterProvider router={router} />);
    await expect.element(page.getByTestId("sessions-empty")).toBeInTheDocument();

    // https -> wss, and the `/v1/realtime` path Factory serves the socket on.
    expect(probe.only().endpoint).toBe("wss://factory.example.test:9443/v1/realtime");
  });

  it("defaults to the same origin that served the page", async () => {
    const composed = compose("/sessions");
    render(<RouterProvider router={composed.router} />);
    await expect.element(page.getByTestId("sessions-empty")).toBeInTheDocument();
    expect(composed.probe.only().endpoint).toBe("/v1/realtime");
  });

  it("reads the build-time override, and treats an unset or blank one as same-origin", () => {
    expect(factoryBaseUrl({ VITE_FACTORY_BASE_URL: "https://factory.example.test:9443" })).toBe(
      "https://factory.example.test:9443",
    );
    expect(factoryBaseUrl({ VITE_FACTORY_BASE_URL: "  https://factory.example.test  " })).toBe(
      "https://factory.example.test",
    );
    expect(factoryBaseUrl({})).toBeUndefined();
    expect(factoryBaseUrl({ VITE_FACTORY_BASE_URL: "   " })).toBeUndefined();
    expect(factoryBaseUrl({ VITE_FACTORY_BASE_URL: 7 })).toBeUndefined();
  });

  it("is what a browser build composes, and it carries no credentials", () => {
    // main.tsx executes exactly this and nothing else; the bootstrap itself
    // mounts into #root on import, so this value is where it can be read.
    expect(browserFactoryComposition({ VITE_FACTORY_BASE_URL: "https://factory.example.test" })).toEqual({
      options: { baseUrl: "https://factory.example.test" },
    });
    expect(browserFactoryComposition({}).options?.baseUrl).toBeUndefined();
    expect(browserFactoryComposition({})).not.toHaveProperty("credentials");
  });
});
