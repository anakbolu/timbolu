"use client";

/**
 * The workspace store behind every screen.
 *
 * Each screen used to fetch its own copy of the same things — the Bolu, the
 * conversations, the drafts and the tasks — which is how the sidebar and the
 * dashboard came to disagree about one workspace. One provider
 * holds them now, and one live connection keeps them current: a view reads from
 * here, and reacts through the actions below.
 *
 * Nothing here is invented. A workspace with no Bolu reads as empty, and a value
 * the API does not have is absent rather than filled in with a plausible number.
 */

import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from "react";
import { useRouter } from "next/navigation";

import {
  createGroup,
  currentUser,
  currentWorkspace,
  decideDraft as decideDraftRequest,
  ensureActiveWorkspace,
  getAccessToken,
  listAgentTeams,
  listApprovalPolicies,
  listConversations,
  listDrafts,
  listHubAdapters,
  listHubGrants,
  listHubIntegrations,
  listMCPServers,
  listMembers,
  listTasks,
  sendMessage,
  setAccessToken,
  type ActivityEvent,
  type Agent,
  type AgentTeam,
  type ApprovalPolicy,
  type ChatConversation,
  type Draft,
  type HubAdapter,
  type HubGrant,
  type HubIntegration,
  type MCPServer,
  type Member,
  type Task,
  type User,
  type Workspace,
} from "@/lib/api";
import { connect, type RealtimeConnection, type RealtimeStatus } from "@/lib/realtime";

/** Hub is the integration hub's reads, kept together because a screen needs them together. */
export type Hub = {
  connections: HubIntegration[];
  adapters: HubAdapter[];
  servers: MCPServer[];
  /** grantsByAgent is keyed by Bolu id: the applications each one may use. */
  grantsByAgent: Record<string, HubGrant[]>;
};

/** Outcome is what an action reports back to the view that asked for it. */
export type Outcome = { ok: boolean; message: string };

export type WorkspaceData = {
  /** ready stays false until the first load finishes, so a view can hold its shape. */
  ready: boolean;
  error: string | null;
  status: RealtimeStatus;
  workspaceID: string | null;
  user: User | null;
  workspace: Workspace | null;
  members: Member[];
  teams: AgentTeam[];
  /** agents is every Bolu in the workspace, flattened across its teams. */
  agents: Agent[];
  conversations: ChatConversation[];
  drafts: Draft[];
  tasks: Task[];
  policies: ApprovalPolicy[];
  hub: Hub;
  /** events is the live feed, newest first, capped to what a screen shows. */
  events: ActivityEvent[];
  /**
   * subscribe attaches a handler to the live stream.
   *
   * The chat needs every message frame as it arrives rather than the capped list
   * of recent events, and it must not open a connection of its own: two sockets
   * per tab would give the same event twice and the last id they resume from
   * would disagree.
   */
  subscribe: (handler: (event: ActivityEvent) => void) => () => void;
  reload: () => Promise<void>;
  reloadAgents: () => Promise<void>;
  reloadConversations: () => Promise<void>;
  reloadDrafts: () => Promise<void>;
  reloadTasks: () => Promise<void>;
  reloadHub: () => Promise<void>;
  reloadPolicies: () => Promise<void>;
  /** decide answers a draft; the dashboard card and the chat card call the same one. */
  decide: (draftId: string, decision: "approve" | "revise" | "cancel", note?: string) => Promise<Outcome>;
  /**
   * sendTask hands one instruction to the team.
   *
   * It goes into the team's group thread, because that is what lets the
   * backend's router choose the Bolu that fits the instruction — a direct thread
   * would answer with whichever Bolu it happens to be with. The group is created
   * on first use.
   */
  sendTask: (instruction: string) => Promise<Outcome>;
};

const FEED_LIMIT = 60;

const WorkspaceContext = createContext<WorkspaceData | null>(null);

/** useWorkspace reads the store. Outside the provider it throws, which is a bug rather than a state. */
export function useWorkspace(): WorkspaceData {
  const value = useContext(WorkspaceContext);
  if (!value) {
    throw new Error("useWorkspace must be used inside WorkspaceProvider");
  }
  return value;
}

export function WorkspaceProvider({ children }: { children: React.ReactNode }) {
  const router = useRouter();
  const [ready, setReady] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [status, setStatus] = useState<RealtimeStatus>("connecting");
  const [user, setUser] = useState<User | null>(null);
  const [workspace, setWorkspace] = useState<Workspace | null>(null);
  const [members, setMembers] = useState<Member[]>([]);
  const [teams, setTeams] = useState<AgentTeam[]>([]);
  const [conversations, setConversations] = useState<ChatConversation[]>([]);
  const [drafts, setDrafts] = useState<Draft[]>([]);
  const [tasks, setTasks] = useState<Task[]>([]);
  const [policies, setPolicies] = useState<ApprovalPolicy[]>([]);
  const [hub, setHub] = useState<Hub>({
    connections: [],
    adapters: [],
    servers: [],
    grantsByAgent: {},
  });
  const [events, setEvents] = useState<ActivityEvent[]>([]);

  // The id is state because the sidebar, the chat and the integrations screen read
  // it while rendering. The ref below mirrors it for the callbacks and the stream,
  // which outlive the render that created them.
  const [workspaceID, setWorkspaceID] = useState<string | null>(null);
  const workspaceIdRef = useRef<string | null>(null);
  const agentsRef = useRef<Agent[]>([]);
  const reloadTimer = useRef<number | undefined>(undefined);
  const subscribers = useRef<Set<(event: ActivityEvent) => void>>(new Set());

  useEffect(() => {
    workspaceIdRef.current = workspaceID;
  }, [workspaceID]);

  const subscribe = useCallback((handler: (event: ActivityEvent) => void) => {
    subscribers.current.add(handler);
    return () => {
      subscribers.current.delete(handler);
    };
  }, []);

  const fail = useCallback((message: string) => {
    setError(message);
  }, []);

  const loadAgents = useCallback(async (id: string) => {
    const result = await listAgentTeams(id);
    if (!result.ok) {
      fail(result.message);
      return;
    }
    setTeams(result.data);
    agentsRef.current = result.data.flatMap((team) => team.agents);
  }, [fail]);

  const loadConversations = useCallback(async (id: string) => {
    const result = await listConversations(id);
    if (!result.ok) {
      fail(result.message);
      return;
    }
    setConversations(result.data);
  }, [fail]);

  const loadDrafts = useCallback(async (id: string) => {
    const result = await listDrafts(id, { limit: 50 });
    if (!result.ok) {
      fail(result.message);
      return;
    }
    setDrafts(result.data);
  }, [fail]);

  const loadTasks = useCallback(async (id: string) => {
    const result = await listTasks(id, { limit: 100 });
    if (!result.ok) {
      fail(result.message);
      return;
    }
    setTasks(result.data);
  }, [fail]);

  // Grants are per Bolu at the API, so the map is assembled from one read per
  // Bolu. It is what lets a connected app say which of them may use it.
  const loadHub = useCallback(async (id: string) => {
    const [connections, adapters, servers, ...grantPages] = await Promise.all([
      listHubIntegrations(id),
      listHubAdapters(id),
      listMCPServers(id),
      ...agentsRef.current.map((agent) => listHubGrants(id, agent.id)),
    ]);

    const grantsByAgent: Record<string, HubGrant[]> = {};
    agentsRef.current.forEach((agent, index) => {
      const page = grantPages[index];
      grantsByAgent[agent.id] = page && page.ok ? page.data : [];
    });

    setHub({
      connections: connections.ok ? connections.data : [],
      adapters: adapters.ok ? adapters.data : [],
      servers: servers.ok ? servers.data : [],
      grantsByAgent,
    });
  }, []);

  const loadPolicies = useCallback(async (id: string) => {
    const result = await listApprovalPolicies(id);
    if (!result.ok) return;
    setPolicies(result.data);
  }, []);

  const reload = useCallback(async () => {
    // A page reload loses the in-memory access token, so the session is restored
    // from the refresh cookie first. `ensureActiveWorkspace` does that refresh
    // itself and stores the token, so it is called once rather than after a second
    // refresh of its own: the cookie is single-use, and asking twice in a row is
    // how a load ends up holding a token the server has already retired.
    const id = await ensureActiveWorkspace();
    if (!id) {
      setReady(true);
      setError("Sesi berakhir. Masuk lagi.");
      // `ensureActiveWorkspace` refreshes first, so returning nothing means either
      // that the refresh failed or that the account has no workspace yet — and the
      // two want opposite answers. Only the first is sent to the door: bouncing the
      // second would put an account with no workspace on a loop between /login and
      // /dashboard, since signing in again lands it right back here.
      //
      // The token says which happened. A failed refresh stores nothing, while a
      // refresh that succeeded but found no workspace leaves a live token.
      if (getAccessToken() === null) {
        router.replace("/login");
      }
      return;
    }
    setWorkspaceID(id);

    const [profile, people, pending, account] = await Promise.all([
      currentWorkspace(id),
      listMembers(id),
      listDrafts(id, { limit: 50 }),
      currentUser(),
    ]);

    if (profile.ok) setWorkspace(profile.data);
    if (people.ok) setMembers(people.data);
    if (pending.ok) setDrafts(pending.data);
    if (account.ok) setUser(account.data);
    else if (account.status === 401) {
      // The API rejected the token, so the session in hand is not one. Dropping it
      // clears the gate's hint with it, and the next navigation is turned away
      // before this shell is sent a second time — which is why this branch leaves
      // the message on screen rather than redirecting: the redirect belongs to the
      // gate, and a second one from here could outrun the cookie write.
      setAccessToken(null);
      setReady(true);
      setError("Sesi berakhir. Masuk lagi.");
      return;
    }

    // The roster is fetched before the hub, because the hub reads grants per Bolu.
    await loadAgents(id);
    await Promise.all([loadConversations(id), loadTasks(id), loadHub(id), loadPolicies(id)]);

    setError(null);
    setReady(true);
  }, [loadAgents, loadConversations, loadHub, loadPolicies, loadTasks, router]);

  useEffect(() => {
    void (async () => {
      await reload();
    })();
  }, [reload]);

  // One live connection for the whole app. Deltas are applied to the lists the
  // changed thing belongs to, and a burst of frames is coalesced: a task emits a
  // step many times a minute.
  useEffect(() => {
    if (!ready || error) return;
    const id = workspaceIdRef.current;
    if (!id) return;

    const scheduleReload = () => {
      clearTimeout(reloadTimer.current);
      reloadTimer.current = window.setTimeout(() => {
        void loadDrafts(id);
        void loadTasks(id);
        void loadConversations(id);
      }, 400);
    };

    const connection: RealtimeConnection = connect(
      id,
      {
        onStatus: setStatus,
        onEvent: (event: ActivityEvent) => {
          setEvents((previous) => [event, ...previous].slice(0, FEED_LIMIT));
          for (const handler of subscribers.current) {
            handler(event);
          }
          switch (event.type) {
            case "draft.created":
            case "draft.decided":
            case "task.started":
            case "task.finished":
            case "task.waiting_approval":
            case "message.new":
              scheduleReload();
              break;
            case "agent.state":
              void loadAgents(id);
              break;
            default:
              break;
          }
        },
      },
      0,
    );

    return () => {
      clearTimeout(reloadTimer.current);
      connection.close();
    };
  }, [ready, error, loadAgents, loadDrafts, loadTasks, loadConversations]);

  const decide = useCallback(
    async (draftId: string, decision: "approve" | "revise" | "cancel", note = "") => {
      const id = workspaceIdRef.current;
      if (!id) return { ok: false, message: "Pilih workspace dulu." };
      const result = await decideDraftRequest(id, draftId, decision, note);
      if (!result.ok) return { ok: false, message: result.message };
      await loadDrafts(id);
      await loadTasks(id);
      return { ok: true, message: "Tersimpan." };
    },
    [loadDrafts, loadTasks],
  );

  const sendTask = useCallback(
    async (instruction: string) => {
      const id = workspaceIdRef.current;
      const text = instruction.trim();
      if (!id) return { ok: false, message: "Pilih workspace dulu." };
      if (!text) return { ok: false, message: "Tulis dulu tugasnya." };

      const workers = agentsRef.current.filter((agent) => agent.status !== "resting");
      if (workers.length === 0) {
        return { ok: false, message: "Belum ada Bolu yang bisa disuruh. Buat satu dulu di halaman Tim." };
      }

      // The team thread is reused when it exists, so repeated instructions land in
      // one conversation rather than making a new group each time.
      const existing = conversations.find(
        (conversation) =>
          conversation.kind === "group" &&
          workers.every((agent) =>
            conversation.participants.some((participant) => participant.agent_id === agent.id),
          ),
      );

      let conversationID = existing?.id;
      if (!conversationID) {
        const created = await createGroup(id, {
          title: "Tim Bolu",
          agent_ids: workers.map((agent) => agent.id),
        });
        if (!created.ok) return { ok: false, message: created.message };
        conversationID = created.data.id;
      }

      const sent = await sendMessage(id, conversationID, { text, reply: true });
      if (!sent.ok) return { ok: false, message: sent.message };

      await loadConversations(id);
      return {
        ok: true,
        message: sent.data.responder
          ? `${sent.data.responder.name} menerimanya.`
          : "Tugas diteruskan ke timmu.",
      };
    },
    [conversations, loadConversations],
  );

  const agents = useMemo(() => teams.flatMap((team) => team.agents), [teams]);

  // The exposed reloads take no argument: the id they act on is the active one, so a
  // caller cannot aim one at another workspace. They are bound here rather than
  // inside the value, because binding them is what reads the id during render.
  const reloadAgents = useCallback(async () => {
    if (workspaceID) await loadAgents(workspaceID);
  }, [workspaceID, loadAgents]);
  const reloadConversations = useCallback(async () => {
    if (workspaceID) await loadConversations(workspaceID);
  }, [workspaceID, loadConversations]);
  const reloadDrafts = useCallback(async () => {
    if (workspaceID) await loadDrafts(workspaceID);
  }, [workspaceID, loadDrafts]);
  const reloadTasks = useCallback(async () => {
    if (workspaceID) await loadTasks(workspaceID);
  }, [workspaceID, loadTasks]);
  const reloadHub = useCallback(async () => {
    if (workspaceID) await loadHub(workspaceID);
  }, [workspaceID, loadHub]);
  const reloadPolicies = useCallback(async () => {
    if (workspaceID) await loadPolicies(workspaceID);
  }, [workspaceID, loadPolicies]);

  const value = useMemo<WorkspaceData>(
    () => ({
      ready,
      error,
      status,
      workspaceID,
      user,
      workspace,
      members,
      teams,
      agents,
      conversations,
      drafts,
      tasks,
      policies,
      hub,
      events,
      subscribe,
      reload,
      reloadAgents,
      reloadConversations,
      reloadDrafts,
      reloadTasks,
      reloadHub,
      reloadPolicies,
      decide,
      sendTask,
    }),
    [
      ready,
      error,
      status,
      workspaceID,
      user,
      workspace,
      members,
      teams,
      agents,
      conversations,
      drafts,
      tasks,
      policies,
      hub,
      events,
      subscribe,
      reload,
      reloadAgents,
      reloadConversations,
      reloadDrafts,
      reloadTasks,
      reloadHub,
      reloadPolicies,
      decide,
      sendTask,
    ],
  );

  return <WorkspaceContext.Provider value={value}>{children}</WorkspaceContext.Provider>;
}
