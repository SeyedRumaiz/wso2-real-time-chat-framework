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
  MenuItem,
  Select,
  type SelectChangeEvent,
} from "@wso2/oxygen-ui";
import type { JSX } from "react";
import {
  useGetEngineerStatus,
  useSetEngineerStatus,
  type EngineerStatus,
} from "@features/csm-chat/api/useEngineerStatus";

// Display info for every status, including PENDING and BUSY — both shown
// (as the current value, with their own color) but never themselves a
// clickable menu item; see SELECTABLE_STATUSES below. PENDING (a case was
// just assigned, awaiting Accept) gets its own blue between Available's
// green and Busy's yellow — a distinct color is the whole point of this
// state existing, so the bar never claims "Busy" before the engineer has
// actually accepted anything.
const STATUS_DISPLAY: Record<EngineerStatus, { label: string; color: string }> = {
  AVAILABLE: { label: "Available", color: "#22C55E" },
  PENDING: { label: "Pending", color: "#3B82F6" },
  BUSY: { label: "Busy", color: "#EAB308" },
  OFFLINE: { label: "Offline", color: "#EF4444" },
};

// The only two states an engineer can ever request directly. PENDING and
// BUSY are purely derived — the routing service sets PENDING the moment a
// case is assigned and BUSY only once the engineer accepts it (see
// chat-routing-service's Router.SetPresence/Escalate/Accept), and a direct
// request for either is now rejected server-side, so neither is offered
// here either.
const SELECTABLE_STATUSES: { value: EngineerStatus; label: string; color: string }[] = [
  { value: "AVAILABLE", ...STATUS_DISPLAY.AVAILABLE },
  { value: "OFFLINE", ...STATUS_DISPLAY.OFFLINE },
];

/**
 * Header dropdown for an engineer's live-chat-routing presence — Available/
 * Busy/Offline (see csm-portal/backend's internal/handler/chat.go
 * HandleSetPresence/HandleGetPresence and the standalone chat-routing-
 * service they proxy to). Modeled on ThemeSelect's plain-Select pattern
 * rather than UserProfile's UserMenu, since this picks a status value, not
 * a person.
 *
 * Defaults to Offline — the routing service's own default for an engineer
 * it has never seen a presence update from — while the initial fetch is in
 * flight, so the dropdown never flashes an incorrect Available state.
 *
 * While PENDING (a case was just assigned, awaiting Accept) or BUSY (an
 * active, accepted chat session), "Available" is shown but disabled — you
 * can't manually jump back to available while a case is reserved for you,
 * capacity is one dedicated case at a time. "Offline" stays enabled:
 * picking it doesn't decline/end anything itself, it just confirms the
 * engineer will go offline once the current pending request or chat
 * completes (pendingOffline). If they never touch it, ending the session
 * returns them straight to AVAILABLE and back into the pool.
 */
export default function EngineerStatusMenu(): JSX.Element {
  const { data: presence } = useGetEngineerStatus();
  const setStatus = useSetEngineerStatus();

  const current: EngineerStatus = presence?.status ?? "OFFLINE";
  const currentOption = STATUS_DISPLAY[current];
  const isPending = current === "PENDING";
  const isBusy = current === "BUSY";

  const handleChange = (e: SelectChangeEvent<string>): void => {
    const next = e.target.value as EngineerStatus;
    if (next === current) return;
    setStatus.mutate(next);
  };

  return (
    <Box sx={{ display: "flex", alignItems: "center" }}>
      <Select
        value={current}
        onChange={handleChange}
        size="small"
        variant="standard"
        disableUnderline
        aria-label="Set your engineer status"
        disabled={setStatus.isPending}
        // Without this, MUI renders the matching MenuItem's children (its
        // own colored dot + label) as the closed-state display *in addition
        // to* the startAdornment dot below -- two dots for one status.
        // renderValue takes over the closed-state display entirely, so only
        // the startAdornment's dot shows once collapsed; each MenuItem's own
        // dot still renders normally in the open dropdown list.
        renderValue={() => currentOption.label}
        startAdornment={
          <Box
            component="span"
            sx={{
              width: 8,
              height: 8,
              borderRadius: "50%",
              bgcolor: currentOption.color,
              mr: 1,
              flexShrink: 0,
            }}
          />
        }
        sx={{
          minWidth: 116,
          fontSize: "0.8125rem",
          color: "text.secondary",
          "& .MuiSelect-select": {
            display: "flex",
            alignItems: "center",
            py: 0.5,
          },
        }}
      >
        {
          // MUI logs an "out-of-range value" warning if the Select's value
          // doesn't match any rendered MenuItem. Neither PENDING nor BUSY
          // is ever a choice, but either can be the *current* value, so
          // each gets a disabled item of its own only while it's actually
          // current -- present so the value always matches something, but
          // never clickable.
          isPending && (
            <MenuItem value="PENDING" disabled sx={{ fontSize: "0.8125rem" }}>
              <Box
                component="span"
                sx={{
                  width: 8,
                  height: 8,
                  borderRadius: "50%",
                  bgcolor: STATUS_DISPLAY.PENDING.color,
                  display: "inline-block",
                  mr: 1,
                }}
              />
              {STATUS_DISPLAY.PENDING.label}
            </MenuItem>
          )
        }
        {
          isBusy && (
            <MenuItem value="BUSY" disabled sx={{ fontSize: "0.8125rem" }}>
              <Box
                component="span"
                sx={{
                  width: 8,
                  height: 8,
                  borderRadius: "50%",
                  bgcolor: STATUS_DISPLAY.BUSY.color,
                  display: "inline-block",
                  mr: 1,
                }}
              />
              {STATUS_DISPLAY.BUSY.label}
            </MenuItem>
          )
        }
        {SELECTABLE_STATUSES.map((o) => (
          <MenuItem
            key={o.value}
            value={o.value}
            // Can't manually go back to Available while a case is
            // reserved (pending or in-progress) -- capacity is one
            // dedicated case at a time. Offline stays enabled: it doesn't
            // decline/end anything itself, it just sets pendingOffline
            // (see this component's doc comment).
            disabled={(isPending || isBusy) && o.value === "AVAILABLE"}
            sx={{ fontSize: "0.8125rem" }}
          >
            <Box
              component="span"
              sx={{
                width: 8,
                height: 8,
                borderRadius: "50%",
                bgcolor: o.color,
                display: "inline-block",
                mr: 1,
              }}
            />
            {o.label}
          </MenuItem>
        ))}
      </Select>
    </Box>
  );
}
