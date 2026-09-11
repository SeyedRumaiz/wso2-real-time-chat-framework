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
  useMutation,
  useQueryClient,
  type UseMutationResult,
} from "@tanstack/react-query";
import { useBackendApi } from "@api/backend/client";
import { ENGINEER_STATUS_QUERY_KEY } from "./useEngineerStatus";

export interface ConvertChatToCaseInput {
  caseId: string;
}

export interface ConvertChatToCaseResult {
  entityCaseId: string;
}

/**
 * Converts a live-engineer-chat session into a real case: POST
 * /chat/sessions/{caseId}/convert-to-case (see csm-portal/backend's
 * internal/handler/chat.go HandleConvertToCase and the project's
 * chat-first-escalation-plan.md). No request body — chat-routing-service
 * already has everything this call needs (subject, customer, message,
 * projectId), stored from the original escalation, so the browser never
 * resends it.
 *
 * Engineer-initiated only (see EngineerAlertNotification's "Convert to
 * Case" button, shown only on an already-accepted session) — the customer
 * never sees this control themselves once a human is on the chat.
 *
 * Unlike useCompleteChatSession, a rejection here is surfaced to the caller,
 * not best-effort: this is a real, synchronous case-creation call (mirrors
 * HandleEscalate's own historical "not best-effort" CreateCase call, just
 * moved to this later trigger) — a failure means either nothing happened
 * (safe to retry) or, in a narrow window, a real case was created but
 * ending the chat session failed server-side, which the engineer needs to
 * know about rather than have silently swallowed. Either way, the caller
 * (EngineerAlertNotification.handleConvertToCase) deliberately does NOT
 * clear the session locally on failure — see that function's own comment.
 *
 * On success, this chat has ended exactly like Completed ends one (see
 * router.Router.ConvertToCase) — same capacity/queue-backfill side effects,
 * delivered the same way (a "customer_escalation" SSE event straight to
 * this engineer if a freed slot was immediately backfilled, no different
 * handling needed here) — so this invalidates the status query the same
 * way useCompleteChatSession/useDeclineChatSession/useSetEngineerStatus do.
 */
export function useConvertChatToCase(): UseMutationResult<
  ConvertChatToCaseResult,
  Error,
  ConvertChatToCaseInput
> {
  const api = useBackendApi();
  const queryClient = useQueryClient();

  return useMutation<ConvertChatToCaseResult, Error, ConvertChatToCaseInput>({
    mutationFn: ({ caseId }) =>
      api.post(`/chat/sessions/${encodeURIComponent(caseId)}/convert-to-case`, {}),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ENGINEER_STATUS_QUERY_KEY });
    },
  });
}
