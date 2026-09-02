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

// Display info for every status, including BUSY — which is shown (as the
// current value, with its own color) but is never itself a clickable menu
// item; see SELECTABLE_STATUSES below.
const STATUS_DISPLAY: Record<EngineerStatus, { label: string; color: string }> = {
  AVAILABLE: { label: "Available", color: "#22C55E" },
  BUSY: { label: "Busy", color: "#EAB308" },
  OFFLINE: { label: "Offline", color: "#EF4444" },
};

// The only two states an engineer can ever request directly. BUSY is purely
// derived — the routing service sets it automatically the moment a case is
// assigned (see chat-routing-service's Router.SetPresence/Escalate), and a
// direct request for it is now rejected server-side, so it's never offered
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
 * While BUSY (an active, accepted chat session), "Available" is shown but
 * disabled — you can't manually leave a session early, capacity is one
 * dedicated case at a time. "Offline" stays enabled: picking it while BUSY
 * doesn't end the session, it just confirms the engineer will go offline
 * once the current chat completes (pendingOffline). If they never touch
 * it, ending the session returns them straight to AVAILABLE and back into
 * the pool.
 */
export default function EngineerStatusMenu(): JSX.Element {
  const { data: status } = useGetEngineerStatus();
  const setStatus = useSetEngineerStatus();

  const current: EngineerStatus = status ?? "OFFLINE";
  const currentOption = STATUS_DISPLAY[current];
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
          // doesn't match any rendered MenuItem. BUSY is never a choice,
          // but it can be the *current* value, so it gets a disabled item
          // of its own only while it's actually current -- present so the
          // value always matches something, but never clickable.
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
            // Can't manually go back to Available mid-session -- capacity
            // is one dedicated case at a time. Offline stays enabled: it
            // doesn't end the session, it just sets pendingOffline (see
            // this component's doc comment).
            disabled={isBusy && o.value === "AVAILABLE"}
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
