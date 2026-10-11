"use client";

/**
 * All dashboard interactivity — ported from the `Component` class in the
 * latest design export ("Bolu — ruang kerja tim"): routing between the five
 * views, bot + group chats, draft verdicts, the live feed timer, routines,
 * the office room, integrations and settings.
 */
import { useCallback, useEffect, useRef, useState } from "react";
import { ALL_BOTS, allBot, withFace, type BotShape } from "@/lib/crew";
import {
  ALL_DASH_BOTS,
  DASH_BOTS,
  DRAFTS,
  GORDER_BOLU,
  GORDER_HORE,
  GROUPS,
  HORE_BOTS,
  INIT_FEED,
  ORDER,
  SEED,
  dashBot,
  type DashBot,
  type DraftId,
  type MsgFrom,
} from "./mock-data";
import { APPS, APP, ICATS, INIT_INTEG, type Integ, type Permission } from "./integrations";
import { NOW_MIN, ROUT, toMin, type Routine } from "./routines";
import {
  BEAN,
  FURN,
  HOME,
  PLACE,
  QUEUE,
  ROUTE,
  SOFA,
  ST,
  type Furniture,
} from "./office";
import {
  AUTO_ROWS,
  DAYS,
  NOTIF_ROWS,
  TONES,
  initialSettings,
  type Settings,
} from "./settings";

/** Accent used by primary buttons, the "today" bar and the now-marker. */
export const ACCENT = "#1F7FD6";

const REPLY_DELAY_MS = 1400;
const FOLLOWUP_DELAY_MS = 1500;
const FEED_INTERVAL_MS = 3000;
const TOAST_MS = 2600;
const CONNECT_DELAY_MS = 1400;
const MAX_FEED_ROWS = 7;
const SLEEP_REPLY = "Zzz… aku lagi istirahat. Nyalakan aku dulu, ya.";

/** Sidebar mini-avatar positions, keyed by group size. */
const MINI_POS: Record<number, [number, number][]> = {
  2: [
    [0, 17],
    [18, 0],
  ],
  3: [
    [0, 18],
    [18, 18],
    [9, 0],
  ],
  4: [
    [0, 0],
    [18, 0],
    [0, 18],
    [18, 18],
  ],
};

export type FeedItem = {
  who: string;
  text: string;
  time: string;
  cls: string;
  you?: boolean;
};

export type ThreadMsg = {
  from: MsgFrom;
  bot: string;
  text: string;
  draft: DraftId | null;
  cls: string;
};

type Verdict = Partial<Record<DraftId, "ok" | "rev">>;

export type ModalState = {
  id: string;
  mode: "new" | "edit";
  step: "form" | "busy" | "done";
  account: string;
  bots: string[];
  perm: Permission;
  error?: string;
};

type DashState = {
  tick: number;
  view: string;
  threads: Record<string, ThreadMsg[]>;
  verdict: Verdict;
  paused: Record<string, boolean>;
  typing: Record<string, string[]>;
  unread: Record<string, number>;
  busyUntil: Record<string, number>;
  chatMsg: string;
  dashMsg: string;
  feed: FeedItem[];
  toast: string;
  toastBot: string;
  /** Bumped on every `say` so the auto-dismiss effect re-runs for repeats. */
  toastSeq: number;
  integ: Record<string, Integ>;
  modal: ModalState | null;
  iCat: string;
  iQuery: string;
  rOff: Record<string, boolean>;
  rFilter: string;
  extraR: Routine[];
  newText: string;
  newTime: string;
  newBot: string;
  horeOpen: boolean;
  navOpen: boolean;
  set: Settings;
};

const VIEW_TITLES: Record<string, string> = {
  dash: "Dasbor",
  routine: "Rutinitas",
  office: "Kantor Tim Bolu",
  integ: "Integrasi",
  settings: "Pengaturan",
};

function initialState(): DashState {
  const threads: Record<string, ThreadMsg[]> = {};
  for (const [name, rows] of Object.entries(SEED)) {
    threads[name] = rows.map(([from, text, draft]) => ({
      from,
      bot: name,
      text,
      draft: draft ?? null,
      cls: "",
    }));
  }
  for (const group of Object.values(GROUPS)) {
    threads[group.id] = group.seed.map(([who, text, draft]) => ({
      from: who === "you" ? "you" : "bot",
      bot: who,
      text,
      draft: draft ?? null,
      cls: "",
    }));
  }
  return {
    tick: 0,
    view: "dash",
    threads,
    verdict: {},
    paused: {},
    typing: {},
    unread: { Oren: 2, Biru: 1, Lila: 1, Kunyit: 1, g1: 1, Riang: 1 },
    busyUntil: {},
    chatMsg: "",
    dashMsg: "",
    feed: INIT_FEED.map(([who, text, time]) => ({ who, text, time, cls: "" })),
    toast: "",
    toastBot: "Oren",
    toastSeq: 0,
    integ: JSON.parse(JSON.stringify(INIT_INTEG)) as Record<string, Integ>,
    modal: null,
    iCat: "Semua",
    iQuery: "",
    rOff: {},
    rFilter: "Semua",
    extraR: [],
    newText: "",
    newTime: "14:00",
    newBot: "Oren",
    horeOpen: true,
    navOpen: false,
    set: initialSettings(),
  };
}

/** Feed clock: one tick every 3s, so half a minute of story time per tick. */
function clock(tick: number): string {
  const minute = 30 + Math.floor(tick / 2);
  return `10.${minute < 60 ? minute : 59}`;
}

function pushFeed(feed: FeedItem[], item: Omit<FeedItem, "time" | "cls">, tick: number): FeedItem[] {
  return [{ ...item, time: clock(tick), cls: "pop" }, ...feed].slice(0, MAX_FEED_ROWS);
}

function addMsg(
  threads: Record<string, ThreadMsg[]>,
  id: string,
  msg: Omit<ThreadMsg, "cls">,
): Record<string, ThreadMsg[]> {
  return { ...threads, [id]: [...(threads[id] ?? []), { ...msg, cls: "pop" }] };
}

/** First bot whose keywords appear in the task; falls back to the first in the pool. */
function route(text: string, pool: DashBot[]): string {
  const low = text.toLowerCase();
  const hit = pool.find((member) => member.kw.some((kw) => low.includes(kw)));
  return (hit ?? pool[0]).name;
}

export type NavItem = {
  id: string;
  label: string;
  /** SVG path data for the 24×24 icon. */
  icon: string;
  on: boolean;
  hasBadge: boolean;
  badge: number;
  pick: () => void;
};

export type GroupSideItem = {
  id: string;
  name: string;
  line: string;
  unread: number;
  hasUnread: boolean;
  on: boolean;
  minis: { key: string; bot: BotShape; left: number; top: number }[];
  pick: () => void;
};

export type SideItem = {
  name: string;
  face: BotShape;
  line: string;
  statusColor: string;
  tint: string;
  hasUnread: boolean;
  unread: number;
  anim: string;
  opacity: number;
  on: boolean;
  pick: () => void;
};

export type StatItem = { label: string; value: string; note: string };

export type FeedRow = {
  key: string;
  who: string;
  text: string;
  time: string;
  cls: string;
  isYou: boolean;
  bot: BotShape;
};

export type WeekBar = { day: string; label: string; h: string; bg: string; weight: number };
export type BillRow = { name: string; no: string; amt: string; status: string; color: string };

export type DraftCard = {
  kind: string;
  title: string;
  draft: string;
  cta: string;
  tint: string;
  fields: { k: string; v: string }[];
  isPending: boolean;
  isDone: boolean;
  doneLabel: string;
  doneBg: string;
  approve: () => void;
  reject: () => void;
};

export type ThreadRow = {
  key: string;
  mine: boolean;
  theirs: boolean;
  text: string;
  cls: string;
  name: string;
  showName: boolean;
  bot: BotShape;
  tint: string;
  hasDraft: boolean;
  card: DraftCard | null;
};

export type RoutineItem = {
  key: string;
  isNow: boolean;
  isItem: boolean;
  time: string;
  title: string;
  freq: string;
  botName: string;
  bot: BotShape;
  status: string;
  statusColor: string;
  bg: string;
  opacity: number;
  on: boolean;
  toggle: () => void;
};

export type RoutineFilter = { label: string; on: boolean; bot: BotShape | null; pick: () => void };

export type OfficeItem = {
  key: string;
  name: string;
  face: BotShape;
  bubble: string;
  where: string;
  statusText: string;
  statusColor: string;
  anim: string;
  left: number;
  top: number;
  z: number;
  pick: () => void;
};

export type ConnectedItem = {
  id: string;
  name: string;
  desc: string;
  tile: string;
  mono: string;
  account: string;
  at: string;
  cls: string;
  users: BotShape[];
  usersText: string;
  permText: string;
  edit: () => void;
  remove: () => void;
};

export type AvailableItem = {
  id: string;
  name: string;
  cat: string;
  desc: string;
  tile: string;
  mono: string;
  users: BotShape[];
  connect: () => void;
};

export type ModalModel = {
  id: string;
  name: string;
  mono: string;
  tile: string;
  desc: string;
  title: string;
  step: "form" | "busy" | "done";
  isForm: boolean;
  isBusy: boolean;
  isDone: boolean;
  account: string;
  accHint: string;
  onAccount: (value: string) => void;
  bots: { name: string; bot: BotShape; on: boolean; pick: () => void }[];
  perms: { label: string; desc: string; on: boolean; pick: () => void }[];
  hasError: boolean;
  error: string;
  note: string;
  cta: string;
  helper: BotShape;
  doneTitle: string;
  doneText: string;
  close: () => void;
  submit: () => void;
};

export type ToggleRow = {
  key: string;
  title: string;
  desc: string;
  bot?: BotShape;
  on: boolean;
  toggle: () => void;
};

export type SettingsModel = {
  ws: string;
  tz: string;
  lang: string;
  start: string;
  end: string;
  onWs: (value: string) => void;
  onTz: (value: string) => void;
  onLang: (value: string) => void;
  onStart: (value: string) => void;
  onEnd: (value: string) => void;
  days: { label: string; on: boolean; pick: () => void }[];
  auto: ToggleRow[];
  tones: { label: string; on: boolean; pick: () => void }[];
  toneSample: string;
  toneBot: BotShape;
  connTiles: { id: string; tile: string; mono: string }[];
  connText: string;
  notif: ToggleRow[];
};

export type ChatModel = {
  isOne: boolean;
  isGroup: boolean;
  title: string;
  bio: string;
  sub: string;
  subColor: string;
  face: BotShape;
  big: BotShape;
  anim: string;
  tint: string;
  color: string;
  members: { name: string; bot: BotShape; role: string; line: string; statusColor: string; anim: string; pick: () => void }[];
  chips: { label: string; pick: () => void }[];
  thread: ThreadRow[];
  typingBots: { key: string; name: string; bot: BotShape; tint: string }[];
  chatMsg: string;
  setChatMsg: (value: string) => void;
  sendChat: () => void;
  placeholder: string;
  doneToday: string;
  pendingN: string;
  skills: string[];
  routines: { time: string; title: string }[];
  noRoutine: boolean;
  off: boolean;
  toggleText: string;
  toggleLabel: string;
  toggle: () => void;
};

export type Dashboard = {
  view: string;
  isDash: boolean;
  isRoutine: boolean;
  isOffice: boolean;
  isInteg: boolean;
  isSettings: boolean;
  isChat: boolean;
  /** Sidebar / shell. */
  logo: BotShape;
  nav: NavItem[];
  openSettings: () => void;
  openInteg: () => void;
  integStyle: { on: boolean };
  integCount: string;
  settingsStyle: { on: boolean };
  groupsBolu: GroupSideItem[];
  sideBolu: SideItem[];
  groupsHore: GroupSideItem[];
  sideHore: SideItem[];
  horeOpen: boolean;
  horeMinis: BotShape[];
  toggleHore: () => void;
  activeLabel: string;
  noPending: boolean;
  pendingCount: number;
  navOpen: boolean;
  toggleNav: () => void;
  closeNav: () => void;
  hasAlert: boolean;
  mTitle: string;
  mIcon: BotShape;
  toast: string;
  toastBot: BotShape;
  /* Dasbor. */
  dashMsg: string;
  onDashMsg: (value: string) => void;
  sendDash: () => void;
  stats: StatItem[];
  feed: FeedRow[];
  week: WeekBar[];
  bills: BillRow[];
  happy: BotShape;
  /* Rutinitas. */
  routineSummary: string;
  rFilters: RoutineFilter[];
  rHourly: RoutineItem[];
  rDaily: RoutineItem[];
  rWeekly: RoutineItem[];
  botNames: string[];
  newText: string;
  newTime: string;
  newBot: string;
  onNewText: (value: string) => void;
  onNewTime: (value: string) => void;
  onNewBot: (value: string) => void;
  addRoutine: () => void;
  /* Kantor. */
  furn: { key: string; label: string; isDesk: boolean; isShelf: boolean; f: Furniture }[];
  office: OfficeItem[];
  /* Integrasi. */
  iQuery: string;
  onIQuery: (value: string) => void;
  iCats: { label: string; on: boolean; pick: () => void }[];
  connLabel: string;
  availLabel: string;
  noConn: boolean;
  noAvail: boolean;
  connected: ConnectedItem[];
  avail: AvailableItem[];
  /* Modal. */
  hasModal: boolean;
  md: ModalModel | null;
  /* Pengaturan. */
  set: SettingsModel;
  /* Obrolan. */
  chat: ChatModel;
};

export function useDashboardState(): Dashboard {
  const [state, setState] = useState<DashState>(initialState);
  const timers = useRef<ReturnType<typeof setTimeout>[]>([]);

  const later = useCallback((fn: () => void, ms: number) => {
    timers.current.push(setTimeout(fn, ms));
  }, []);

  // Live feed: every other tick one busy bot reports a finished act.
  useEffect(() => {
    const id = setInterval(() => {
      setState((prev) => {
        const tick = prev.tick + 1;
        const active = DASH_BOTS.filter((member) => {
          if (prev.paused[member.name]) return false;
          const until = prev.busyUntil[member.name] ?? 0;
          if (until > tick) return true;
          return (Math.floor(tick / 4) + ALL_DASH_BOTS.indexOf(member)) % 3 !== 0;
        });
        if (tick % 2 !== 0 || active.length === 0) return { ...prev, tick };
        const member = active[(tick / 2) % active.length];
        const act = member.acts[(tick / 2) % member.acts.length];
        return {
          ...prev,
          tick,
          feed: pushFeed(prev.feed, { who: member.name, text: `selesai ${act}` }, tick),
        };
      });
    }, FEED_INTERVAL_MS);
    return () => clearInterval(id);
  }, []);

  useEffect(() => () => {
    for (const timer of timers.current) clearTimeout(timer);
  }, []);

  // Auto-dismiss the toast; `toastSeq` makes repeated identical toasts re-arm.
  useEffect(() => {
    if (!state.toastSeq) return;
    const id = setTimeout(() => setState((prev) => ({ ...prev, toast: "" })), TOAST_MS);
    return () => clearTimeout(id);
  }, [state.toastSeq]);

  const say = useCallback((text: string, who: string) => {
    setState((prev) => ({ ...prev, toast: text, toastBot: who, toastSeq: prev.toastSeq + 1 }));
  }, []);

  const open = useCallback((id: string) => {
    setState((prev) => ({ ...prev, view: id, unread: { ...prev.unread, [id]: 0 }, chatMsg: "", navOpen: false }));
  }, []);

  const go = useCallback((view: string) => {
    setState((prev) => ({ ...prev, view, navOpen: false }));
  }, []);

  /** The two-step bot reply inside one conversation. */
  const botReply = useCallback(
    (convo: string, name: string, text: string, follow: boolean) => {
      setState((prev) => {
        const asleep = !!prev.paused[name];
        const member = dashBot(name);
        const reply = asleep
          ? SLEEP_REPLY
          : follow
            ? `Siap, aku bantu dari sisi ${member.role.toLowerCase()}. ${member.after}`
            : `Oke! Aku kerjakan: “${text}”. ${member.after}`;
        const busyUntil = { ...prev.busyUntil };
        if (!asleep) busyUntil[name] = prev.tick + 6;
        const unread = { ...prev.unread };
        if (prev.view !== convo) unread[convo] = (unread[convo] ?? 0) + 1;
        const feed =
          !asleep && !follow && DASH_BOTS.some((b) => b.name === name)
            ? pushFeed(prev.feed, { who: name, text: `menerima tugas: “${text}”` }, prev.tick)
            : prev.feed;
        return {
          ...prev,
          threads: addMsg(prev.threads, convo, { from: "bot", bot: name, text: reply, draft: null }),
          busyUntil,
          unread,
          feed,
        };
      });
    },
    [],
  );

  const talk = useCallback(
    (convo: string, raw: string) => {
      const text = raw.trim();
      if (!text) return;
      const group = GROUPS[convo];
      const pool = group ? group.members.map((n) => dashBot(n)) : [dashBot(convo)];
      const first = route(text, pool);
      const second = group ? group.members.filter((n) => n !== first)[0] : null;
      setState((prev) => ({
        ...prev,
        threads: addMsg(prev.threads, convo, { from: "you", bot: convo, text, draft: null }),
        typing: { ...prev.typing, [convo]: [first] },
        chatMsg: "",
      }));
      later(() => {
        botReply(convo, first, text, false);
        setState((prev) => ({ ...prev, typing: { ...prev.typing, [convo]: second ? [second] : [] } }));
        if (second) {
          later(() => {
            botReply(convo, second, text, true);
            setState((prev) => ({ ...prev, typing: { ...prev.typing, [convo]: [] } }));
          }, FOLLOWUP_DELAY_MS);
        }
      }, REPLY_DELAY_MS);
    },
    [botReply, later],
  );

  const decide = useCallback(
    (id: DraftId, ok: boolean) => {
      const draft = DRAFTS[id];
      setState((prev) => ({
        ...prev,
        verdict: { ...prev.verdict, [id]: ok ? "ok" : "rev" },
        threads: addMsg(prev.threads, draft.bot, {
          from: "bot",
          bot: draft.bot,
          text: ok
            ? "Beres, sudah kukirim. Terima kasih!"
            : "Oke, aku revisi dulu ya. Nanti kukabari lagi di sini.",
          draft: null,
        }),
        feed: pushFeed(
          prev.feed,
          {
            who: "Kamu",
            you: true,
            text: ok ? `menyetujui ${draft.title}` : `minta ${draft.bot} merevisi ${draft.title}`,
          },
          prev.tick,
        ),
      }));
      say(ok ? `${draft.title} terkirim` : `${draft.bot} akan merevisi drafnya`, draft.bot);
    },
    [say],
  );

  const togglePause = useCallback(
    (name: string, off: boolean) => {
      setState((prev) => ({ ...prev, paused: { ...prev.paused, [name]: !off } }));
      say(off ? `${name} kembali bekerja` : `${name} istirahat dulu`, name);
    },
    [say],
  );

  const sendDash = useCallback(() => {
    const text = state.dashMsg.trim();
    if (!text) return;
    const who = route(text, DASH_BOTS);
    setState((prev) => ({ ...prev, dashMsg: "" }));
    talk(who, text);
    say(`Diteruskan ke ${who}`, who);
  }, [say, state.dashMsg, talk]);

  const sendChat = useCallback(() => {
    talk(state.view, state.chatMsg);
  }, [state.chatMsg, state.view, talk]);

  const addRoutine = useCallback(() => {
    const title = state.newText.trim();
    if (!title) {
      say("Tulis dulu apa yang perlu dikerjakan", "Lila");
      return;
    }
    const time = (state.newTime || "14:00").replace(":", ".");
    const bot = state.newBot || "Oren";
    const routine: Routine = { id: `x${Date.now()}`, slot: "daily", time, bot, title, freq: "Setiap hari" };
    setState((prev) => ({ ...prev, extraR: [...prev.extraR, routine], newText: "" }));
    say(`${bot} akan mengerjakannya tiap hari pukul ${time}`, bot);
  }, [say, state.newBot, state.newText, state.newTime]);

  const openModal = useCallback((modal: ModalState) => {
    setState((prev) => ({ ...prev, modal }));
  }, []);

  const patchModal = useCallback((patch: Partial<ModalState>) => {
    setState((prev) => (prev.modal ? { ...prev, modal: { ...prev.modal, ...patch } } : prev));
  }, []);

  const closeModal = useCallback(() => setState((prev) => ({ ...prev, modal: null })), []);

  const submitModal = useCallback(() => {
    setState((prev) => {
      const modal = prev.modal;
      if (!modal) return prev;
      const app = APP[modal.id];
      if (!modal.account.trim()) {
        return { ...prev, modal: { ...modal, error: "Isi dulu akun yang mau disambungkan." } };
      }
      if (modal.bots.length === 0) {
        return {
          ...prev,
          modal: { ...modal, error: `Pilih minimal satu Bolu yang boleh memakai ${app.name}.` },
        };
      }
      if (modal.mode === "edit") {
        return {
          ...prev,
          integ: {
            ...prev.integ,
            [modal.id]: { ...prev.integ[modal.id], account: modal.account.trim(), bots: modal.bots, perm: modal.perm },
          },
          modal: null,
        };
      }
      return { ...prev, modal: { ...modal, step: "busy" } };
    });

    const modal = state.modal;
    if (!modal || modal.mode === "edit") {
      if (modal && modal.account.trim() && modal.bots.length) {
        say(`Akses ${APP[modal.id].name} disimpan`, modal.bots[0]);
      }
      return;
    }
    if (!modal.account.trim() || modal.bots.length === 0) return;

    const app = APP[modal.id];
    later(() => {
      setState((prev) => {
        if (!prev.modal) return prev;
        const who = prev.modal.bots[0];
        return {
          ...prev,
          integ: {
            ...prev.integ,
            [prev.modal.id]: {
              account: prev.modal.account.trim(),
              bots: prev.modal.bots,
              perm: prev.modal.perm,
              at: clock(prev.tick),
              fresh: true,
            },
          },
          modal: { ...prev.modal, step: "done" },
          threads: addMsg(prev.threads, who, {
            from: "bot",
            bot: who,
            text: `Akses ke ${app.name} sudah aktif. Aku siap memakainya!`,
            draft: null,
          }),
        };
      });
      say(`${app.name} terhubung`, modal.bots[0]);
    }, CONNECT_DELAY_MS);
  }, [later, say, state.modal]);

  const { tick, view, threads, verdict, paused, typing, unread } = state;

  const pend = ORDER.filter((id) => !verdict[id]);
  const waitBy: Record<string, number> = {};
  for (const id of pend) {
    const owner = DRAFTS[id].bot;
    waitBy[owner] = (waitBy[owner] ?? 0) + 1;
  }
  const doneCount = Object.keys(verdict).length;

  function isTyping(name: string): boolean {
    return Object.values(typing).some((list) => list.includes(name));
  }

  function isBusy(name: string, t = tick): boolean {
    const member = dashBot(name);
    const until = state.busyUntil[name] ?? 0;
    if (until > t) return true;
    return (Math.floor(t / 4) + ALL_DASH_BOTS.indexOf(member)) % 3 !== 0;
  }

  function chilling(name: string): boolean {
    return !paused[name] && !isTyping(name) && !waitBy[name] && !isBusy(name);
  }

  function face(name: string, fx = false): BotShape {
    const member = allBot(name);
    if (paused[name]) return withFace(member, { mouth: "sleep", closed: true });
    if (isTyping(name)) return withFace(member, { mouth: "focus", look: "translate(3 -3)" });
    if (chilling(name)) return withFace(member, { lazy: true, mouth: "chill", fx });
    return member;
  }

  function status(name: string): [string, string, string] {
    if (paused[name]) return ["Istirahat", "#8B88A0", "Istirahat"];
    if (isTyping(name)) return ["Sedang mengetik…", "#1E7A43", "Sedang mengetik…"];
    if (waitBy[name]) return ["Menunggu kamu", "#E2602B", "Draf menunggu kamu"];
    if (chilling(name)) return ["Lagi santai, siap terima tugas", "#4A86E8", "Lagi santai"];
    const member = dashBot(name);
    const act =
      member.acts[(Math.floor(tick / 2) + ALL_DASH_BOTS.indexOf(member)) % member.acts.length];
    return ["Sedang bekerja", "#2FA65A", `Sedang ${act}`];
  }

  function animOf(name: string): string {
    return paused[name] ? "" : chilling(name) ? "sway" : "bob";
  }

  const isDash = view === "dash";
  const isRoutine = view === "routine";
  const isOffice = view === "office";
  const isInteg = view === "integ";
  const isSettings = view === "settings";
  const isChat = !isDash && !isRoutine && !isOffice && !isInteg && !isSettings;

  const NAV: { id: string; label: string; icon: string }[] = [
    { id: "dash", label: "Dasbor", icon: "M4 4h7v7H4zM13 4h7v7h-7zM4 13h7v7H4zM13 13h7v7h-7z" },
    { id: "routine", label: "Rutinitas", icon: "M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18zM12 7v5l3 2" },
    { id: "office", label: "Kantor", icon: "M3 21V8l9-5 9 5v13M9 21v-6h6v6M3 21h18" },
  ];

  const nav: NavItem[] = NAV.map((item) => {
    const on = view === item.id;
    return {
      id: item.id,
      label: item.label,
      icon: item.icon,
      on,
      hasBadge: item.id === "dash" && pend.length > 0,
      badge: pend.length,
      pick: () => go(item.id),
    };
  });

  const sideList = (list: DashBot[]): SideItem[] =>
    list.map((member) => {
      const [, statusColor, line] = status(member.name);
      const on = view === member.name;
      const count = unread[member.name] ?? 0;
      return {
        name: member.name,
        face: face(member.name),
        line,
        statusColor,
        tint: member.tint,
        hasUnread: count > 0 && !on,
        unread: count,
        anim: animOf(member.name),
        opacity: paused[member.name] ? 0.5 : 1,
        on,
        pick: () => open(member.name),
      };
    });

  const groupList = (ids: string[]): GroupSideItem[] =>
    ids.map((id) => {
      const group = GROUPS[id];
      const on = view === id;
      const count = unread[id] ?? 0;
      const last = (threads[id] ?? []).slice(-1)[0];
      const ty = (typing[id] ?? [])[0];
      const line = ty
        ? `${ty} sedang mengetik…`
        : last
          ? last.from === "you"
            ? `Kamu: ${last.text}`
            : `${last.bot}: ${last.text}`
          : group.members.join(", ");
      const positions = MINI_POS[group.members.length] ?? MINI_POS[4];
      return {
        id,
        name: group.name,
        line,
        unread: count,
        hasUnread: count > 0 && !on,
        on,
        minis: group.members.map((name, i) => ({
          key: name,
          bot: face(name),
          left: positions[i][0],
          top: positions[i][1],
        })),
        pick: () => open(id),
      };
    });

  const toast = state.toast;

  const shared = {
    view,
    isDash,
    isRoutine,
    isOffice,
    isInteg,
    isSettings,
    isChat,
    logo: allBot("Oren"),
    nav,
    openSettings: () => go("settings"),
    openInteg: () => go("integ"),
    integStyle: { on: isInteg },
    integCount: `${Object.keys(state.integ).length} aktif`,
    settingsStyle: { on: isSettings },
    groupsBolu: groupList(GORDER_BOLU),
    sideBolu: sideList(DASH_BOTS),
    groupsHore: groupList(GORDER_HORE),
    sideHore: sideList(HORE_BOTS),
    horeOpen: state.horeOpen,
    horeMinis: HORE_BOTS.map((b) => face(b.name)),
    toggleHore: () => setState((prev) => ({ ...prev, horeOpen: !prev.horeOpen })),
    activeLabel: `${DASH_BOTS.filter((b) => !paused[b.name]).length} aktif`,
    noPending: pend.length === 0,
    pendingCount: pend.length,
    navOpen: state.navOpen,
    toggleNav: () => setState((prev) => ({ ...prev, navOpen: !prev.navOpen })),
    closeNav: () => setState((prev) => ({ ...prev, navOpen: false })),
    hasAlert:
      pend.length > 0 || Object.keys(unread).some((key) => unread[key] > 0 && key !== view),
    mTitle: VIEW_TITLES[view] ?? GROUPS[view]?.name ?? view,
    mIcon: GROUPS[view] ? face(GROUPS[view].members[0]) : isChat ? face(view) : allBot("Oren"),
    toast,
    toastBot: allBot(state.toastBot),
  };

  /* -------------------------------------------------------------- Rutinitas */
  const allR = [...ROUT, ...state.extraR];
  const rFilter = state.rFilter;
  const shown = allR.filter((r) => rFilter === "Semua" || r.bot === rFilter);

  function rItem(r: Routine): RoutineItem {
    const off = !!state.rOff[r.id];
    let st: string;
    let col: string;
    if (off) {
      st = "Dimatikan";
      col = "#8B88A0";
    } else if (r.slot === "hourly") {
      st = "Berjalan · berikutnya 11.00";
      col = "#1E7A43";
    } else if (r.slot === "weekly") {
      st = `Nanti, ${r.time}`;
      col = "#5E5B70";
    } else if (toMin(r.time) <= NOW_MIN) {
      const done = r.time.replace(/\.(\d\d)$/, (_m, x: string) => `.${`0${parseInt(x, 10) + 3}`.slice(-2)}`);
      st = `Selesai ${done}${r.last ? ` · ${r.last}` : ""}`;
      col = "#1E7A43";
    } else {
      st = `Nanti, ${r.time}`;
      col = "#5E5B70";
    }
    return {
      key: r.id,
      isNow: false,
      isItem: true,
      time: r.time,
      title: r.title,
      freq: r.freq,
      botName: r.bot,
      bot: face(r.bot),
      status: st,
      statusColor: col,
      bg: off ? "#F5F6FB" : "#FFFFFF",
      opacity: off ? 0.6 : 1,
      on: !off,
      toggle: () => {
        setState((prev) => ({ ...prev, rOff: { ...prev.rOff, [r.id]: !off } }));
        say(off ? `Rutinitas ${r.bot} dinyalakan` : `Rutinitas ${r.bot} dimatikan`, r.bot);
      },
    };
  }

  const daily = shown
    .filter((r) => r.slot === "daily")
    .sort((a, b) => toMin(a.time) - toMin(b.time));
  const rDaily: RoutineItem[] = [];
  let placed = false;
  for (const r of daily) {
    if (!placed && toMin(r.time) > NOW_MIN) {
      rDaily.push({ key: "now", isNow: true, isItem: false, time: "10.30", title: "", freq: "", botName: "", bot: allBot("Oren"), status: "", statusColor: "", bg: "", opacity: 1, on: true, toggle: () => {} });
      placed = true;
    }
    rDaily.push(rItem(r));
  }
  if (!placed) {
    rDaily.push({ key: "now", isNow: true, isItem: false, time: "10.30", title: "", freq: "", botName: "", bot: allBot("Oren"), status: "", statusColor: "", bg: "", opacity: 1, on: true, toggle: () => {} });
  }
  const activeN = allR.filter((r) => !state.rOff[r.id]).length;
  const doneN = allR.filter((r) => !state.rOff[r.id] && r.slot === "daily" && toMin(r.time) <= NOW_MIN).length;

  const rFilters: RoutineFilter[] = ["Semua", ...DASH_BOTS.map((b) => b.name)].map((label) => ({
    label,
    on: rFilter === label,
    bot: label === "Semua" ? null : allBot(label),
    pick: () => setState((prev) => ({ ...prev, rFilter: label })),
  }));

  /* ----------------------------------------------------------------- Kantor */
  const furn = FURN.map((f, i) => ({
    key: `f${i}`,
    label: f.label,
    isDesk: !!f.desk,
    isShelf: !!f.shelf,
    f,
  }));

  let sofaI = 0;
  let queueI = 0;
  let beanI = 0;
  const office: OfficeItem[] = DASH_BOTS.map((b, i) => {
    const name = b.name;
    const [, statusColor, statusText] = status(name);
    let pos: [number, number];
    let bubble: string;
    let where: string;
    let fx = false;
    if (paused[name]) {
      pos = BEAN[beanI++ % BEAN.length];
      bubble = "Zzz…";
      where = "tidur di bean bag";
    } else if (isTyping(name)) {
      pos = ST[HOME[name]];
      bubble = "mengetik balasan…";
      where = `di ${PLACE[HOME[name]]}`;
    } else if (waitBy[name] && (Math.floor(tick / 3) + i) % 3 === 0) {
      pos = QUEUE[queueI++ % QUEUE.length];
      bubble = "tunggu persetujuanmu";
      where = "antre di meja persetujuanmu";
    } else if (chilling(name)) {
      pos = SOFA[sofaI++ % SOFA.length];
      bubble = ["ngeteh dulu", "dengerin musik", "istirahat sebentar", "ngopi"][i % 4];
      where = "santai di pojok sofa";
      fx = true;
    } else {
      const routeSteps = ROUTE[name];
      const stop = routeSteps[(Math.floor(tick / 2) + i) % routeSteps.length];
      const p = ST[stop[0]];
      pos = stop[0] === HOME[name] ? p : [p[0] + 6, p[1] + 2];
      bubble = stop[1];
      where = `di ${PLACE[stop[0]]}`;
    }
    return {
      key: name,
      name,
      face: face(name, fx),
      bubble,
      where,
      statusText,
      statusColor,
      anim: animOf(name),
      left: pos[0],
      top: pos[1],
      z: Math.round(pos[1]),
      pick: () => open(name),
    };
  });

  /* --------------------------------------------------------------- Integrasi */
  const cat = state.iCat;
  const q = state.iQuery.toLowerCase().trim();
  const matches = (app: (typeof APPS)[number]) =>
    (cat === "Semua" || app.cat === cat) &&
    (!q || `${app.name} ${app.desc} ${app.cat}`.toLowerCase().includes(q));
  const connApps = APPS.filter((app) => state.integ[app.id] && matches(app));
  const availApps = APPS.filter((app) => !state.integ[app.id] && matches(app));

  const connected: ConnectedItem[] = connApps.map((app) => {
    const integ = state.integ[app.id];
    return {
      id: app.id,
      name: app.name,
      desc: app.desc,
      tile: app.tile,
      mono: app.mono,
      account: integ.account,
      at: integ.at,
      cls: integ.fresh ? "pop" : "",
      users: integ.bots.map((n) => face(n)),
      usersText: integ.bots.join(", "),
      permText: integ.perm === "r" ? "baca saja" : "baca dan tulis",
      edit: () =>
        openModal({
          id: app.id,
          mode: "edit",
          step: "form",
          account: integ.account,
          bots: [...integ.bots],
          perm: integ.perm,
        }),
      remove: () => {
        setState((prev) => {
          const next = { ...prev.integ };
          delete next[app.id];
          return { ...prev, integ: next };
        });
        say(`${app.name} diputuskan`, integ.bots[0]);
      },
    };
  });

  const avail: AvailableItem[] = availApps.map((app) => ({
    id: app.id,
    name: app.name,
    cat: app.cat,
    desc: app.desc,
    tile: app.tile,
    mono: app.mono,
    users: app.bots.map((n) => face(n)),
    connect: () =>
      openModal({
        id: app.id,
        mode: "new",
        step: "form",
        account: app.google ? "bisnis@[DOMAIN]" : "",
        bots: [...app.bots],
        perm: "rw",
      }),
  }));

  const iCats = ICATS.map((label) => ({
    label,
    on: cat === label,
    pick: () => setState((prev) => ({ ...prev, iCat: label })),
  }));

  /* ------------------------------------------------------------------ Modal */
  let md: ModalModel | null = null;
  if (state.modal) {
    const modal = state.modal;
    const app = APP[modal.id];
    const helperName = modal.bots[0] || app.bots[0];
    md = {
      id: app.id,
      name: app.name,
      mono: app.mono,
      tile: app.tile,
      desc: app.desc,
      title: modal.mode === "edit" ? `Atur akses ${app.name}` : `Hubungkan ${app.name}`,
      step: modal.step,
      isForm: modal.step === "form",
      isBusy: modal.step === "busy",
      isDone: modal.step === "done",
      account: modal.account,
      accHint: app.google || app.id === "outlook" ? "alamat email akun" : "nama akun atau nomor",
      onAccount: (value) => patchModal({ account: value, error: "" }),
      bots: ALL_DASH_BOTS.map((b) => {
        const on = modal.bots.includes(b.name);
        return {
          name: b.name,
          bot: allBot(b.name),
          on,
          pick: () => {
            const list = modal.bots.includes(b.name)
              ? modal.bots.filter((n) => n !== b.name)
              : [...modal.bots, b.name];
            patchModal({ bots: list, error: "" });
          },
        };
      }),
      perms: [
        { key: "r" as Permission, label: "Baca saja", desc: "melihat data tanpa mengubah" },
        { key: "rw" as Permission, label: "Baca dan tulis", desc: "bisa membuat dan mengubah data" },
      ].map((p) => ({ label: p.label, desc: p.desc, on: modal.perm === p.key, pick: () => patchModal({ perm: p.key }) })),
      hasError: !!modal.error,
      error: modal.error ?? "",
      note:
        modal.mode === "edit"
          ? "Perubahan akses berlaku untuk pekerjaan berikutnya."
          : `Setelah ini kamu akan diarahkan ke halaman login ${app.name} untuk memberi izin.`,
      cta: modal.mode === "edit" ? "Simpan" : "Lanjutkan",
      helper: face(helperName),
      doneTitle: `${app.name} terhubung`,
      doneText: `${modal.bots.join(", ")} sudah bisa memakai ${app.name}. Kamu bisa mengubah aksesnya kapan saja.`,
      close: closeModal,
      submit: submitModal,
    };
  }

  /* -------------------------------------------------------------- Pengaturan */
  const S = state.set;
  const upd = (key: keyof Settings, value: Settings[keyof Settings]) =>
    setState((prev) => ({ ...prev, set: { ...prev.set, [key]: value } }));
  const sw = (group: "auto" | "notif", key: string, title: string, desc: string, bot?: string): ToggleRow => {
    const on = !!(S[group] as Record<string, boolean>)[key];
    return {
      key,
      title,
      desc,
      bot: bot ? allBot(bot) : undefined,
      on,
      toggle: () =>
        setState((prev) => ({
          ...prev,
          set: { ...prev.set, [group]: { ...prev.set[group], [key]: !on } },
        })),
    };
  };

  const set: SettingsModel = {
    ws: S.ws,
    tz: S.tz,
    lang: S.lang,
    start: S.start,
    end: S.end,
    onWs: (value) => upd("ws", value),
    onTz: (value) => upd("tz", value),
    onLang: (value) => upd("lang", value),
    onStart: (value) => upd("start", value),
    onEnd: (value) => upd("end", value),
    days: DAYS.map((label) => ({
      label,
      on: !!S.days[label],
      pick: () => setState((prev) => ({ ...prev, set: { ...prev.set, days: { ...prev.set.days, [label]: !prev.set.days[label] } } })),
    })),
    auto: AUTO_ROWS.map((row) => sw("auto", row.key, row.title, row.desc, row.bot)),
    tones: Object.keys(TONES).map((label) => ({
      label,
      on: S.tone === label,
      pick: () => upd("tone", label),
    })),
    toneSample: TONES[S.tone] ?? TONES.Ramah,
    toneBot: allBot("Biru"),
    connTiles: Object.keys(state.integ).map((id) => ({ id, tile: APP[id].tile, mono: APP[id].mono })),
    connText: `${Object.keys(state.integ).length} aplikasi terhubung: ${Object.keys(state.integ)
      .map((id) => APP[id].name)
      .join(", ")}`,
    notif: NOTIF_ROWS.map((row) => sw("notif", row.key, row.title, row.desc)),
  };

  /* ---------------------------------------------------------------- Obrolan */
  const group = isChat ? GROUPS[view] : undefined;
  let chat: ChatModel;
  if (group) {
    const [, subColor, sub] = ["", "#5E5B70", `${group.members.length} anggota: ${group.members.join(", ")}`];
    chat = {
      isOne: false,
      isGroup: true,
      title: group.name,
      bio: group.desc,
      sub,
      subColor,
      face: allBot(group.members[0]),
      big: allBot("Oren"),
      anim: "",
      tint: "#EEF0F6",
      color: "#1E1B2E",
      members: group.members.map((name) => {
        const [, statusColor, line] = status(name);
        return {
          name,
          bot: face(name),
          role: dashBot(name).role,
          line,
          statusColor,
          anim: animOf(name),
          pick: () => open(name),
        };
      }),
      chips: group.chips.map((label) => ({ label, pick: () => talk(view, label) })),
      thread: (threads[view] ?? []).map((msg, index) => threadRow(msg, index, true)),
      typingBots: (typing[view] ?? []).map((name) => ({
        key: name,
        name,
        bot: face(name),
        tint: allBot(name).tint,
      })),
      chatMsg: state.chatMsg,
      setChatMsg: (value) => setState((prev) => ({ ...prev, chatMsg: value })),
      sendChat,
      placeholder: `Tulis ke ${group.name}…`,
      doneToday: "",
      pendingN: "",
      skills: [],
      routines: [],
      noRoutine: true,
      off: false,
      toggleText: "",
      toggleLabel: "",
      toggle: () => {},
    };
  } else if (isChat) {
    const member = dashBot(view);
    const off = !!paused[view];
    const [, statusColor, sub] = status(view);
    const approved = ORDER.filter((id) => DRAFTS[id].bot === view && verdict[id] === "ok").length;
    const myR = allR.filter((r) => r.bot === view);
    const big = off
      ? withFace(member, { mouth: "sleep", closed: true })
      : isTyping(view)
        ? withFace(member, { mouth: "focus", look: "translate(4 -4)" })
        : chilling(view)
          ? withFace(member, { lazy: true, mouth: "chill", fx: true })
          : withFace(member, { mouth: "open", look: "translate(0 2)" });
    chat = {
      isOne: true,
      isGroup: false,
      title: member.name,
      bio: member.bio,
      sub,
      subColor: statusColor,
      face: face(view),
      big,
      anim: animOf(view),
      tint: member.tint,
      color: member.color,
      members: [],
      chips: member.chips.map((label) => ({ label, pick: () => talk(view, label) })),
      thread: (threads[view] ?? []).map((msg, index) => threadRow(msg, index, false)),
      typingBots: (typing[view] ?? []).map((name) => ({
        key: name,
        name,
        bot: face(name),
        tint: allBot(name).tint,
      })),
      chatMsg: state.chatMsg,
      setChatMsg: (value) => setState((prev) => ({ ...prev, chatMsg: value })),
      sendChat,
      placeholder: `Suruh atau tanya ${member.name}…`,
      doneToday: String(member.done + approved),
      pendingN: String(waitBy[view] ?? 0),
      skills: member.skills,
      routines: myR.map((r) => ({ time: r.time, title: r.title })),
      noRoutine: myR.length === 0,
      off,
      toggleText: off ? "Istirahat" : "Aktif",
      toggleLabel: `${off ? "Nyalakan " : "Hentikan sementara "}${member.name}`,
      toggle: () => togglePause(view, off),
    };
  } else {
    chat = {
      isOne: false,
      isGroup: false,
      title: "",
      bio: "",
      sub: "",
      subColor: "#5E5B70",
      face: allBot("Oren"),
      big: allBot("Oren"),
      anim: "",
      tint: "#EEF0F6",
      color: "#1E1B2E",
      members: [],
      chips: [],
      thread: [],
      typingBots: [],
      chatMsg: "",
      setChatMsg: () => {},
      sendChat: () => {},
      placeholder: "",
      doneToday: "",
      pendingN: "",
      skills: [],
      routines: [],
      noRoutine: true,
      off: false,
      toggleText: "",
      toggleLabel: "",
      toggle: () => {},
    };
  }

  function threadRow(msg: ThreadMsg, index: number, isGroupThread: boolean): ThreadRow {
    const known = (name: string) => ALL_BOTS.some((member) => member.name === name);
    const botName = msg.bot && known(msg.bot) ? msg.bot : view;
    return {
      key: `${view}-${index}`,
      mine: msg.from === "you",
      theirs: msg.from === "bot",
      text: msg.text,
      cls: msg.cls,
      name: botName,
      showName: isGroupThread,
      bot: known(botName) ? face(botName) : allBot("Oren"),
      tint: known(botName) ? allBot(botName).tint : "#EEF0F6",
      hasDraft: !!msg.draft,
      card: msg.draft ? cardFor(msg.draft) : null,
    };
  }

  function cardFor(id: DraftId): DraftCard {
    const draft = DRAFTS[id];
    const decided = verdict[id];
    return {
      kind: draft.kind,
      title: draft.title,
      draft: draft.draft,
      cta: draft.cta,
      tint: allBot(draft.bot).tint,
      fields: draft.fields.map(([k, v]) => ({ k, v })),
      isPending: !decided,
      isDone: !!decided,
      doneLabel: decided === "ok" ? "Disetujui dan terkirim" : "Diminta revisi",
      doneBg: decided === "ok" ? "#1E7A43" : "#B4421F",
      approve: () => decide(id, true),
      reject: () => decide(id, false),
    };
  }

  return {
    ...shared,
    dashMsg: state.dashMsg,
    onDashMsg: (value) => setState((prev) => ({ ...prev, dashMsg: value })),
    sendDash,
    stats: [
      {
        label: "Tugas selesai hari ini",
        value: String(77 + Math.floor(tick / 2) + doneCount),
        note: "oleh 6 anggota tim",
      },
      { label: "Perkiraan waktu dihemat", value: "± 3 jam", note: "dibanding dikerjakan manual" },
      { label: "Tagihan belum dibayar", value: "Rp 2,4 jt", note: "5 invoice · 2 lewat tempo" },
    ],
    feed: state.feed.map((item, index) => ({
      key: `${item.time}-${item.who}-${index}`,
      who: item.who,
      text: item.text,
      time: item.time,
      cls: item.cls,
      isYou: !!item.you,
      bot: item.you ? allBot("Oren") : allBot(item.who),
    })),
    week: (
      [
        ["Sen", 86],
        ["Sel", 94],
        ["Rab", 77 + Math.floor(tick / 2) + doneCount],
        ["Kam", 0],
        ["Jum", 0],
        ["Sab", 0],
        ["Min", 0],
      ] satisfies [string, number][]
    ).map(([day, value], index) => ({
      day: String(day),
      label: value ? String(value) : "",
      h: value ? `${Math.round(Math.min(1, value / 110) * 140)}px` : "6px",
      bg: index === 2 ? ACCENT : value ? "#1E1B2E" : "#E3E5EF",
      weight: index === 2 ? 700 : 500,
    })),
    bills: [
      {
        name: "Toko Sari",
        no: "INV-0043",
        amt: "Rp 1.124.000",
        status: verdict.a1 === "ok" ? "Terkirim" : "Menunggu kamu",
        color: verdict.a1 === "ok" ? "#5E5B70" : "#B4421F",
      },
      { name: "Dimas", no: "INV-0041", amt: "Rp 245.000", status: "Belum dibayar", color: "#5E5B70" },
      { name: "Bagas", no: "INV-0039", amt: "Rp 189.000", status: "Lewat 3 hari", color: "#B4421F" },
      { name: "Rina", no: "INV-0042", amt: "Rp 170.000", status: "Lunas", color: "#1E7A43" },
    ],
    happy: withFace(allBot("Pinky"), { mouth: "open" }),
    routineSummary: `${activeN} rutinitas aktif, ${doneN} sudah jalan hari ini.`,
    rFilters,
    rHourly: shown.filter((r) => r.slot === "hourly").map(rItem),
    rDaily,
    rWeekly: shown.filter((r) => r.slot === "weekly").map(rItem),
    botNames: DASH_BOTS.map((b) => b.name),
    newText: state.newText,
    newTime: state.newTime,
    newBot: state.newBot,
    onNewText: (value) => setState((prev) => ({ ...prev, newText: value })),
    onNewTime: (value) => setState((prev) => ({ ...prev, newTime: value })),
    onNewBot: (value) => setState((prev) => ({ ...prev, newBot: value })),
    addRoutine,
    furn,
    office,
    iQuery: state.iQuery,
    onIQuery: (value) => setState((prev) => ({ ...prev, iQuery: value })),
    iCats,
    connLabel: `${connApps.length} aplikasi`,
    availLabel: `${availApps.length} aplikasi`,
    noConn: connApps.length === 0,
    noAvail: availApps.length === 0 && !!q,
    connected,
    avail,
    hasModal: !!state.modal,
    md,
    set,
    chat,
  };
}
