// Copyright (c) 2026 WSO2 LLC. (https://www.wso2.com).
//
// WSO2 LLC. licenses this file to you under the Apache License,
// Version 2.0 (the "License"); you may not use this file except
// in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

import {
  Box,
  Button,
  CircularProgress,
  IconButton,
  LinearProgress,
  Paper,
  Stack,
  TextField,
  Typography,
} from "@wso2/oxygen-ui";
import { useCallback, useEffect, useState, type JSX, type KeyboardEvent } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { BackendApiError } from "@api/backend/client";
import { useIdTokenClaims } from "@hooks/useIdTokenClaims";
import { useChatAlertsStream } from "@features/csm-chat/api/useChatAlertsStream";
import { DEFAULT_PENDING_TIMEOUT_SECONDS } from "@features/csm-chat/api/useEngineerStatus";
import { useAcceptChatSession } from "@features/csm-chat/api/useAcceptChatSession";
import { useSendChatMessage } from "@features/csm-chat/api/useSendChatMessage";
import { useCompleteChatSession } from "@features/csm-chat/api/useCompleteChatSession";
import { useDeclineChatSession } from "@features/csm-chat/api/useDeclineChatSession";
import {
  ENGINEER_STATUS_QUERY_KEY,
  useGetEngineerStatus,
  type EngineerPresence,
} from "@features/csm-chat/api/useEngineerStatus";
import type { ChatAlertEvent } from "@features/csm-chat/types/chatAlerts";

type PendingAlert = {
  caseId: string;
  conversationId: string;
  projectId?: string;
  subject?: string;
  customerEmail?: string;
  customerName?: string;
  message?: string;
  // ISO 8601 -- when this engineer was assigned this case (from the SSE
  // event's own timestamp for a fresh assignment, or from GetPresence's
  // pendingSince when rehydrating after a refresh). Drives the accept-
  // countdown below.
  assignedAt: string;
};

type LiveChatMessage = {
  id: string;
  from: "customer" | "engineer";
  text: string;
};

type ActiveSession = {
  caseId: string;
  conversationId: string;
  customerName?: string;
  messages: LiveChatMessage[];
};

/**
 * App-wide floating widget for the live-engineer-chat escalation feature —
 * see csm-portal/backend's internal/handler/chat.go for the full design.
 * Mounted once in AuthGuard.tsx (alongside IdleTimeoutProvider) so it's
 * visible on every page for any signed-in engineer, independent of which
 * route they're on.
 *
 * Holds exactly one pending alert and one active session at a time — a
 * deliberate simplification matching this feature's other accepted
 * shortcuts (see the backend doc comment: no queue, no claim/lock table).
 * Any escalation beyond the one currently shown is silently ignored by
 * handleAlert until the current one clears; at this project's current
 * scale (a handful of engineers) that is an acceptable limitation, not a
 * bug — a fuller "chat inbox" is a natural follow-up if this ever matters.
 *
 * Renders nothing (returns null) whenever there is neither a pending alert
 * nor an active session, so it never occupies space or blocks a click when
 * idle.
 */
export default function EngineerAlertNotification(): JSX.Element | null {
  const myEmail = useIdTokenClaims()?.email;
  const [pending, setPending] = useState<PendingAlert | null>(null);
  const [session, setSession] = useState<ActiveSession | null>(null);
  const [messageDraft, setMessageDraft] = useState("");

  const acceptMutation = useAcceptChatSession();
  const sendMutation = useSendChatMessage();
  const completeMutation = useCompleteChatSession();
  const declineMutation = useDeclineChatSession();
  const queryClient = useQueryClient();
  const { data: presence } = useGetEngineerStatus();

  // Ticks once a second, only while a pending alert is showing, to drive
  // the accept-countdown rendered below -- see PendingAlert.assignedAt and
  // presence.pendingTimeoutSeconds (chat-routing-service's configured
  // PENDING_TIMEOUT_SECONDS). Purely a UI countdown: the real timeout is
  // enforced server-side on its own poll cadence (see that service's
  // SweepExpiredPending and csm-portal/backend's StartTimeoutSweeper), so
  // this can briefly read a few seconds past zero before the case_timed_
  // out/session_accepted event for it actually arrives and clears it.
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!pending) return;
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, [pending]);

  const pendingTimeoutSeconds = presence?.pendingTimeoutSeconds ?? DEFAULT_PENDING_TIMEOUT_SECONDS;
  const remainingSeconds = pending
    ? Math.max(
        0,
        pendingTimeoutSeconds - Math.floor((now - new Date(pending.assignedAt).getTime()) / 1000),
      )
    : null;

  // Clears currentCase in the cached presence the instant a session/alert
  // is locally dismissed (handleComplete/handleDismiss), rather than
  // waiting for the mutation's own invalidateQueries to trigger a refetch.
  // Without this, there's a window — between clearing local state here and
  // that refetch actually resolving — where this query's cached data is
  // still the OLD value (status BUSY/PENDING, currentCase set), and the
  // rehydrate effect right below fires on exactly that stale read (its
  // deps include `presence`, and pending/session just became null),
  // resurrecting the very session/alert that was just dismissed. That was
  // the "End Session button comes back, and clicking it again drops you to
  // Available instead of Offline" bug: the resurrected widget let
  // handleComplete fire a *second* POST .../complete for the same
  // already-ended case, and chat-routing-service's Completed() -- now
  // guarded against this specifically, see its own doc comment -- used to
  // silently re-derive AVAILABLE/OFFLINE from pending_offline's value
  // *after* the first call had already reset it. This fixes the cause on
  // the UI side; the router-level guard fixes it defensively either way.
  const clearCachedCurrentCase = useCallback((): void => {
    queryClient.setQueryData<EngineerPresence | undefined>(
      ENGINEER_STATUS_QUERY_KEY,
      (prev) => (prev ? { ...prev, currentCase: undefined } : prev),
    );
  }, [queryClient]);

  // Rehydrates a lost alert/session after a refresh or a remount of this
  // component (it's mounted once in AuthGuard, but a full page reload
  // still wipes pending/session, which live only in this component's own
  // state — see the doc comment on this whole component). GetPresence's
  // status now distinguishes PENDING (assigned, not yet accepted) from
  // BUSY (already accepted, chat in progress) — see chat-routing-service's
  // router.Router.Accept — so this rehydrates straight into the matching
  // local state instead of always guessing "pending" the way it had to
  // before that distinction existed: PENDING becomes a pending alert
  // (Accept/Decline still to come), BUSY goes directly into an active
  // session with an empty message history (any messages exchanged before
  // the reload are still in the case's comment history server-side, just
  // not replayed into this local transcript). Either way, this is what
  // gets an engineer un-stuck who is genuinely still PENDING/BUSY
  // server-side with nothing left in the UI to act on.
  useEffect(() => {
    if (pending || session || !presence?.currentCase) return;
    const cc = presence.currentCase;
    if (presence.status === "BUSY") {
      setSession({
        caseId: cc.caseId,
        conversationId: cc.conversationId,
        customerName: cc.customerName,
        messages: [],
      });
      return;
    }
    setPending({
      caseId: cc.caseId,
      conversationId: cc.conversationId,
      subject: cc.subject,
      customerEmail: cc.customerEmail,
      customerName: cc.customerName,
      message: cc.message,
      // Falls back to "now" only if the backend somehow omitted
      // pendingSince for a PENDING engineer, which SweepExpiredPending's
      // own status check should make impossible -- this just avoids a
      // broken/NaN countdown rather than silently trusting bad data.
      assignedAt: presence.pendingSince ?? new Date().toISOString(),
    });
  }, [pending, session, presence]);

  const handleAlert = useCallback(
    (event: ChatAlertEvent) => {
      switch (event.type) {
        case "customer_escalation": {
          if (!event.caseId || !event.conversationId) return;
          // Ignore new escalations while already showing one, or while a
          // session is live — see this component's own doc comment.
          if (session) return;
          // Receiving this event at all means the routing service just
          // assigned this case to us — we're PENDING server-side from this
          // instant, before Accept is even clicked (see router.Router.
          // Accept, which is what later flips this to BUSY). The status
          // dropdown's query has no way to know that on its own (nothing
          // pushes to it), so invalidate it here rather than leaving it
          // stuck showing whatever it was cached as (usually Available).
          queryClient.invalidateQueries({ queryKey: ENGINEER_STATUS_QUERY_KEY });
          setPending((current) => {
            if (current) return current;
            return {
              caseId: event.caseId as string,
              conversationId: event.conversationId as string,
              projectId: event.projectId,
              subject: event.subject,
              customerEmail: event.customerEmail,
              customerName: event.customerName,
              message: event.message,
              assignedAt: event.timestamp,
            };
          });
          break;
        }
        case "session_accepted": {
          // Someone else took this case — our own accept already
          // transitions us to the active session locally (see
          // handleAccept), so only clear when a *different* engineer's
          // email comes back.
          setPending((current) =>
            current &&
            current.caseId === event.caseId &&
            event.engineerEmail !== myEmail
              ? null
              : current,
          );
          break;
        }
        case "customer_message": {
          setSession((current) => {
            if (!current || current.caseId !== event.caseId) return current;
            return {
              ...current,
              messages: [
                ...current.messages,
                {
                  id: `customer-${event.timestamp}-${current.messages.length}`,
                  from: "customer",
                  text: event.message ?? "",
                },
              ],
            };
          });
          break;
        }
        case "session_closed": {
          setSession((current) =>
            current && current.caseId === event.caseId ? null : current,
          );
          setPending((current) =>
            current && current.caseId === event.caseId ? null : current,
          );
          break;
        }
        case "case_timed_out": {
          // We never accepted this one in time -- chat-routing-service
          // already reassigned/requeued it and took us OFFLINE server-side
          // (see that service's SweepExpiredPending). Only ever applies to
          // a still-pending alert, never an active session.
          setPending((current) =>
            current && current.caseId === event.caseId ? null : current,
          );
          queryClient.invalidateQueries({ queryKey: ENGINEER_STATUS_QUERY_KEY });
          break;
        }
        default:
          break;
      }
    },
    [session, myEmail],
  );

  // Always subscribed (enabled: true) for any signed-in engineer — there is
  // no per-page opt-in, since an escalation can arrive while an engineer is
  // anywhere in the app. A no-op when CSM_PORTAL_CHAT_STREAM_BASE_URL isn't
  // configured — see useChatAlertsStream's own doc comment.
  useChatAlertsStream(true, handleAlert);

  const handleAccept = useCallback(async (): Promise<void> => {
    if (!pending) return;
    const { caseId, conversationId, customerName } = pending;
    try {
      await acceptMutation.mutateAsync({ caseId, conversationId });
      setSession({ caseId, conversationId, customerName, messages: [] });
      setPending(null);
    } catch (err) {
      // A 409 means the routing service's Accept check found this case
      // isn't PENDING-for-this-engineer anymore (see HandleAcceptSession) —
      // it was declined, reassigned, or already accepted elsewhere while
      // this alert sat on screen. Nothing to retry there, so clear it
      // rather than leaving a stuck "Accept" button that will only ever
      // fail again. Any other failure (network blip, routing service
      // briefly down) leaves the alert visible so the engineer can retry,
      // or another engineer's session_accepted clears it above.
      if (err instanceof BackendApiError && err.status === 409) {
        setPending(null);
      }
    }
  }, [pending, acceptMutation]);

  // Unlike handleComplete (clears immediately, decline is best-effort after),
  // this awaits the decline call BEFORE clearing: escalations are now routed
  // to exactly one engineer, so dismissing without telling the routing
  // service would otherwise strand the customer with nobody else ever
  // seeing their request (see useDeclineChatSession's own doc comment). The
  // widget still clears locally even if the call fails — no worse than
  // today's un-routed dismiss.
  const handleDismiss = useCallback(async (): Promise<void> => {
    const current = pending;
    if (current) {
      try {
        await declineMutation.mutateAsync({
          caseId: current.caseId,
          conversationId: current.conversationId,
        });
      } catch {
        // Best-effort — still clear locally below either way.
      }
    }
    setPending(null);
    clearCachedCurrentCase();
  }, [pending, declineMutation, clearCachedCurrentCase]);

  const handleSend = useCallback(async (): Promise<void> => {
    const text = messageDraft.trim();
    if (!text || !session) return;
    setMessageDraft("");
    const { caseId, conversationId } = session;
    setSession((current) =>
      current
        ? {
            ...current,
            messages: [
              ...current.messages,
              { id: `engineer-${Date.now()}`, from: "engineer", text },
            ],
          }
        : current,
    );
    try {
      await sendMutation.mutateAsync({ caseId, conversationId, message: text });
    } catch {
      // Best-effort optimistic send — a failure just means the customer
      // never saw this one; the engineer can retype it.
    }
  }, [messageDraft, session, sendMutation]);

  const handleComplete = useCallback(async (): Promise<void> => {
    if (!session) return;
    const { caseId, conversationId } = session;
    setSession(null);
    clearCachedCurrentCase();
    try {
      await completeMutation.mutateAsync({ caseId, conversationId });
    } catch {
      // Best-effort — the widget has already cleared locally either way.
    }
  }, [session, completeMutation, clearCachedCurrentCase]);

  const handleDraftKeyDown = useCallback(
    (e: KeyboardEvent<HTMLDivElement>): void => {
      if (e.key === "Enter" && !e.shiftKey) {
        e.preventDefault();
        void handleSend();
      }
    },
    [handleSend],
  );

  if (!pending && !session) return null;

  return (
    <Paper
      elevation={4}
      sx={{
        position: "fixed",
        bottom: 24,
        right: 24,
        width: 340,
        maxWidth: "calc(100vw - 48px)",
        zIndex: 1400,
        display: "flex",
        flexDirection: "column",
        overflow: "hidden",
      }}
    >
      {pending && (
        <Box sx={{ p: 2 }}>
          <Stack direction="row" alignItems="flex-start" justifyContent="space-between">
            <Typography variant="subtitle2" fontWeight={600}>
              Live engineer requested
            </Typography>
            <IconButton
              size="small"
              aria-label="Dismiss"
              onClick={() => void handleDismiss()}
              disabled={acceptMutation.isPending || declineMutation.isPending}
            >
              <Typography component="span" sx={{ fontSize: "1rem", lineHeight: 1 }}>
                &times;
              </Typography>
            </IconButton>
          </Stack>
          {remainingSeconds !== null && (
            <Box sx={{ mt: 1 }}>
              <LinearProgress
                variant="determinate"
                value={Math.min(100, (remainingSeconds / pendingTimeoutSeconds) * 100)}
                color={remainingSeconds <= 10 ? "warning" : "primary"}
                sx={{ height: 4, borderRadius: 2 }}
              />
              <Typography
                variant="caption"
                color="text.secondary"
                sx={{ mt: 0.5, display: "block" }}
              >
                {remainingSeconds > 0
                  ? `Auto-reassigns in ${remainingSeconds}s if not accepted`
                  : "Reassigning any moment\u2026"}
              </Typography>
            </Box>
          )}
          <Typography variant="body2" color="text.secondary" sx={{ mt: 0.5 }}>
            {pending.customerName || pending.customerEmail || "A customer"} is
            asking to talk to a live engineer.
          </Typography>
          {pending.message && (
            <Typography
              variant="body2"
              sx={{
                mt: 1,
                p: 1,
                bgcolor: "action.hover",
                borderRadius: 1,
                whiteSpace: "pre-wrap",
                overflowWrap: "anywhere",
              }}
            >
              {pending.message}
            </Typography>
          )}
          <Stack direction="row" spacing={1} sx={{ mt: 1.5 }}>
            <Button
              variant="contained"
              color="primary"
              size="small"
              onClick={() => void handleAccept()}
              disabled={acceptMutation.isPending}
              startIcon={
                acceptMutation.isPending ? (
                  <CircularProgress size={14} color="inherit" />
                ) : undefined
              }
              sx={{ textTransform: "none" }}
            >
              Accept
            </Button>
            <Button
              variant="text"
              size="small"
              onClick={() => void handleDismiss()}
              disabled={acceptMutation.isPending || declineMutation.isPending}
              sx={{ textTransform: "none" }}
            >
              Dismiss
            </Button>
          </Stack>
        </Box>
      )}

      {session && (
        <>
          <Box sx={{ p: 1.5, borderBottom: 1, borderColor: "divider" }}>
            <Stack direction="row" alignItems="center" justifyContent="space-between">
              <Typography variant="subtitle2" fontWeight={600} noWrap sx={{ pr: 1 }}>
                {session.customerName || "Live chat"}
              </Typography>
              <Button
                size="small"
                variant="outlined"
                color="inherit"
                onClick={() => void handleComplete()}
                disabled={completeMutation.isPending}
                sx={{ textTransform: "none" }}
              >
                End session
              </Button>
            </Stack>
          </Box>
          <Box
            sx={{
              maxHeight: 260,
              overflowY: "auto",
              p: 1.5,
              display: "flex",
              flexDirection: "column",
              gap: 1,
            }}
          >
            {session.messages.length === 0 ? (
              <Typography variant="caption" color="text.secondary">
                No messages yet — say hello.
              </Typography>
            ) : (
              session.messages.map((m) => (
                <Box
                  key={m.id}
                  sx={{
                    alignSelf: m.from === "engineer" ? "flex-end" : "flex-start",
                    maxWidth: "85%",
                  }}
                >
                  <Typography
                    variant="body2"
                    sx={{
                      p: 1,
                      borderRadius: 1,
                      whiteSpace: "pre-wrap",
                      overflowWrap: "anywhere",
                      bgcolor:
                        m.from === "engineer" ? "primary.main" : "action.hover",
                      color:
                        m.from === "engineer"
                          ? "primary.contrastText"
                          : "text.primary",
                    }}
                  >
                    {m.text}
                  </Typography>
                </Box>
              ))
            )}
          </Box>
          <Box sx={{ p: 1.5, borderTop: 1, borderColor: "divider" }}>
            <Stack direction="row" spacing={1}>
              <TextField
                size="small"
                fullWidth
                placeholder="Type a message..."
                value={messageDraft}
                onChange={(e) => setMessageDraft(e.target.value)}
                onKeyDown={handleDraftKeyDown}
                multiline
                maxRows={3}
              />
              <Button
                variant="contained"
                size="small"
                onClick={() => void handleSend()}
                disabled={!messageDraft.trim() || sendMutation.isPending}
                sx={{ textTransform: "none" }}
              >
                Send
              </Button>
            </Stack>
          </Box>
        </>
      )}
    </Paper>
  );
}
