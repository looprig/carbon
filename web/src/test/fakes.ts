import { RealtimeTransportError } from "@looprig/client";
import type { FactoryPlane } from "./factory-plane.js";
import type { ClientLink, ClientLinkConstructor, ClientLinkCredentials, ClientLinkOptions, ClientLinkState, ClientSubscription, CommandStatus, FactoryClientOptions, FactoryPublication, FetchLike, SessionReset, SubscribeOptions, VersionNegotiationResponse, RecentSessionPage } from "@looprig/client";

export interface FakeSubscription extends ClientSubscription {
  readonly sessionId: string;
  readonly options: SubscribeOptions;
  readonly unsubscribeCount: number;
  deliver(publication: FactoryPublication): void;
  reset(reset: SessionReset): void;
  fail(error: Error): void;
}

export class FakeClientLink implements ClientLink {
  state: ClientLinkState = "disconnected";
  /** One entry per connect attempt that reached the application's token function. */
  readonly connectTokens: string[] = [];
  readonly rpcCalls: Array<{ method: string; request: unknown }> = [];
  connectCalls = 0;
  disconnectCalls = 0;
  readonly subscriptions: FakeSubscription[] = [];

  get open(): FakeSubscription[] {
    return this.subscriptions.filter((subscription) => subscription.state !== "unsubscribed");
  }

  readonly endpoint: string | undefined;
  readonly credentials: ClientLinkCredentials;

  #pending: { promise: Promise<VersionNegotiationResponse>; settle(): void; fail(reason: unknown): void } | undefined;
  /**
   * While true, an authorized connect stops one step short of `connected` and
   * waits for `settleConnect()`. That window is the only place a test can put
   * work "between the cold REST capture and realtime authorization", which is
   * what a browser refresh against a moving journal actually is.
   */
  holdConnect = false;
  #held: (() => void) | undefined;

  /** Completes a connect that `holdConnect` parked. A no-op if none is parked. */
  settleConnect(): void {
    const held = this.#held;
    this.#held = undefined;
    held?.();
  }

  constructor(
    options: ClientLinkOptions,
    private readonly probe: FactoryLinkProbe | undefined = undefined,
  ) {
    this.endpoint = options.endpoint;
    this.credentials = options.credentials ?? {};
  }

  connect(): Promise<VersionNegotiationResponse> {
    this.connectCalls += 1;
    if (this.state === "connected") return Promise.resolve(NEGOTIATED_VERSION);
    if (this.#pending !== undefined) return this.#pending.promise;

    let resolve!: (value: VersionNegotiationResponse) => void;
    let reject!: (reason: unknown) => void;
    const promise = new Promise<VersionNegotiationResponse>((res, rej) => {
      resolve = res;
      reject = rej;
    });
    const attempt = {
      promise,
      settle: (): void => resolve(NEGOTIATED_VERSION),
      fail: (reason: unknown): void => reject(reason),
    };
    this.#pending = attempt;
    this.state = "connecting";

    const mint = this.credentials.connectionToken;
    const token = mint === undefined
      ? Promise.resolve(undefined)
      : mint().then((value) => {
        this.connectTokens.push(value);
        return value;
      });
    void token.then(
      () => {
        if (this.#pending !== attempt) return;
        const proceed = (): void => {
          if (this.#pending !== attempt) return;
          this.#pending = undefined;
          this.state = "connected";
          this.probe?.opened();
          attempt.settle();
        };
        if (this.holdConnect) {
          this.#held = proceed;
          return;
        }
        proceed();
      },
      (error: unknown) => {
        if (this.#pending !== attempt) return;
        this.#pending = undefined;
        this.state = "disconnected";
        attempt.fail(error);
      },
    );
    return promise;
  }

  disconnect(): void {
    this.disconnectCalls += 1;
    this.#held = undefined;
    const wasConnected = this.state === "connected";
    this.state = "disconnected";
    const pending = this.#pending;
    this.#pending = undefined;
    pending?.fail(new RealtimeTransportError("connection closed"));
    if (wasConnected) this.probe?.closed();
  }

  subscribe(options: SubscribeOptions): ClientSubscription {
    if (this.open.some((subscription) => subscription.sessionId === options.sessionId)) {
      throw new Error(`duplicate session subscription: ${options.sessionId}`);
    }
    let state: "subscribed" | "unsubscribed" = "subscribed";
    const subscription = {
      sessionId: options.sessionId,
      // Retained so a test can push what the socket would have pushed. These
      // three are the ONLY way a publication reaches the application in a test,
      // which is what makes "a superseded subscription's frames render nothing"
      // an assertion about the consumer rather than about this fake declining
      // to call it.
      options,
      unsubscribeCount: 0,
      ready: Promise.resolve(),
      version: 1,
      get state() { return state; },
      deliver(publication: FactoryPublication) { options.onPublication(publication); },
      reset(reset: SessionReset) { options.onReset(reset); },
      /**
       * Ends this subscription the way a lost transport does: the channel is
       * released, so the same session may be subscribed again, and the error
       * reaches the binding rather than the returned `ready` promise.
       */
      fail(error: Error) {
        if (state === "unsubscribed") return;
        state = "unsubscribed";
        options.onError?.(error);
      },
      unsubscribe() {
        if (state === "unsubscribed") return;
        state = "unsubscribed";
        subscription.unsubscribeCount += 1;
      },
    } satisfies FakeSubscription;
    this.subscriptions.push(subscription);
    return subscription;
  }

  /**
   * Loses the connection underneath every open subscription.
   *
   * This is the app-level shape of a Factory REPLICA change: the socket to one
   * replica goes away, the client reconnects — to whichever replica it is
   * routed to next — and every binding must rejoin and repair from its own
   * committed coverage rather than from anything the old replica held. The link
   * is left disconnected, so the next `connect()` mints a fresh attempt exactly
   * as a reconnect does.
   */
  drop(reason = "connection lost"): void {
    this.#held = undefined;
    if (this.state === "connected") this.probe?.closed();
    this.state = "disconnected";
    for (const subscription of [...this.subscriptions]) {
      if (subscription.state !== "unsubscribed") subscription.fail(new Error(reason));
    }
  }

  rpc(method: string, request: unknown): Promise<CommandStatus> {
    this.rpcCalls.push({ method, request });
    return new Promise<CommandStatus>(() => {});
  }
}

const NEGOTIATED_VERSION: VersionNegotiationResponse = { version: 1 };

/**
 * Counts the Factory links an application constructs, and how many are open at
 * once.
 *
 * "One WebSocket per app" is a NEGATIVE assertion, so the counter has to be
 * able to observe two: `router.test.tsx` renders two applications over one
 * probe and reads `links.length === 2` and `maxOpen === 2` before it asserts
 * that one application reaches 1. The link is where the socket lives —
 * `createClientLink` constructs the Centrifuge client, which owns the socket —
 * so counting constructions counts sockets.
 *
 * `maxOpen` is the concurrency bound rather than a total: a reconnect is one
 * socket after another, not two at once, and `connectCalls` on a link is how a
 * test reads reconnects.
 */
export class FactoryLinkProbe {
  readonly links: FakeClientLink[] = [];
  readonly fetchCalls: Array<{ input: string; init?: RequestInit }> = [];
  open = 0;
  maxOpen = 0;
  bootstrapResult: Promise<Response> = Promise.resolve(new Response(
    JSON.stringify({ tenant_id: "tenant-1" }),
    { headers: { "Cache-Control": "no-store", "Content-Type": "application/json" } },
  ));
  recentSessionsResult: Promise<RecentSessionPage> = Promise.resolve({ sessions: [] });
  /**
   * The durable read plane, for a test that needs one answering per session
   * rather than a fixed page. See `./factory-plane.ts`; unset, the canned
   * responses below still answer.
   */
  plane: FactoryPlane | undefined = undefined;
  /**
   * Runs on each link the moment it is constructed, before the provider's
   * effect connects it. A test that must arm `holdConnect` has no other window:
   * the link is built inside `createFactoryClient`, which the provider calls
   * itself, so there is no handle to reach for until after `connect()`.
   */
  clientLinkHook: ((link: FakeClientLink) => void) | undefined = undefined;

  /** The exact `ClientLinkConstructor` `createFactoryClient` takes. */
  readonly clientLinkFactory: ClientLinkConstructor = (options: ClientLinkOptions = {}): ClientLink => {
    const link = new FakeClientLink(options, this);
    this.links.push(link);
    this.clientLinkHook?.(link);
    return link;
  };

  /**
   * A `fetch` that records and never settles. `FactoryRestReads` and
   * `createFactoryCommands` both take one; nothing in `app/` issues a Factory
   * REST request before U5.2, so a call here is a finding, and a never-settling
   * promise is the one shape that neither swallows it nor manufactures an
   * unhandled rejection.
   */
  readonly fetch: FetchLike = (input: string, init?: RequestInit): Promise<Response> => {
    this.fetchCalls.push(init === undefined ? { input } : { input, init });
    // A programmable durable plane, when one is installed, answers the session
    // read routes AHEAD of the canned ones below. It answers only routes it
    // owns, so bootstrap and the recent-session list stay here.
    const planned = this.plane?.respond(new URL(input, "https://factory.invalid"), init);
    if (planned !== undefined) return planned;
    if (new URL(input, "https://factory.invalid").pathname === "/v1/bootstrap") {
      // StrictMode runs the bootstrap effect twice. A Response body is
      // one-shot, so every HTTP call must receive its own response instance.
      return this.bootstrapResult.then((response) => response.clone());
    }
    if (new URL(input, "https://factory.invalid").pathname === "/v1/sessions") {
      return this.recentSessionsResult.then((page) => new Response(JSON.stringify(page)));
    }
    const url = new URL(input, "https://factory.invalid");
    const status = /^\/v1\/sessions\/([^/]+)\/status$/.exec(url.pathname);
    if (status !== null) return Promise.resolve(new Response(JSON.stringify({
      session_id: decodeURIComponent(status[1]!), agent_id: "agent-1", state: "idle", residency: "cold", journal_tip: 0,
    })));
    if (/^\/v1\/sessions\/[^/]+\/gates$/.test(url.pathname)) {
      return Promise.resolve(new Response(JSON.stringify({ journal_tip: 0, open_gate_count: 0, gates: [] })));
    }
    if (/^\/v1\/sessions\/[^/]+\/journal$/.test(url.pathname)) {
      return Promise.resolve(new Response(JSON.stringify({ journal_tip: 0, covered_through: 0, events: [] })));
    }
    return new Promise<Response>(() => {});
  };

  /** Everything `createAppRouter` needs to compose a Factory client over this probe. */
  options(overrides: Partial<FactoryClientOptions> = {}): Omit<FactoryClientOptions, "credentials"> {
    return { clientLinkFactory: this.clientLinkFactory, fetch: this.fetch, ...overrides };
  }

  /** The one link this application built, once its provider's effect has run. */
  only(): FakeClientLink {
    if (this.links.length !== 1) {
      throw new Error(`FactoryLinkProbe: expected exactly one link, saw ${this.links.length}`);
    }
    return this.links[0]!;
  }

  opened(): void {
    this.open += 1;
    if (this.open > this.maxOpen) this.maxOpen = this.open;
  }

  closed(): void {
    this.open -= 1;
  }
}
