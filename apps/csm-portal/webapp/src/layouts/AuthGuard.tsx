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

import { type JSX, useEffect } from "react";
import { useAsgardeo } from "@asgardeo/react";
import { ProtectedRoute } from "@asgardeo/react-router";
import { Outlet, useLocation, useNavigate } from "react-router";
import AppLayout from "@layouts/AppLayout";
import { POST_LOGIN_REDIRECT_KEY } from "@layouts/postLoginRedirect";
import { CurrentUserProvider } from "@context/current-user/CurrentUserContext";
import EngineerAlertNotification from "@features/csm-chat/components/EngineerAlertNotification";

/**
 * AuthGuard renders AppLayout (header/footer) unconditionally, then gates only
 * the routed content (CurrentUserProvider + the current page's <Outlet/>)
 * behind ProtectedRoute. AppLayout is mounted exactly once here — it has its
 * own internal loading UI (`hasInitialized`) driven by the same auth state, so
 * it doesn't need to be swapped out by ProtectedRoute the way earlier versions
 * of this file did. See the comment above `protectedContent` below for why
 * that mattered.
 *
 * Preserves the intended URL across the IdP sign-in redirect so that
 * deep-links (e.g. ServiceNow case links) land on the correct page after auth.
 *
 * Note: the customer-portal behaviour of auto-redirecting `/` to the last
 * visited project's dashboard is intentionally NOT replicated here. CSM is
 * engineer-scoped, so the landing route `/` resolves to the ABT dashboard
 * via App.tsx instead.
 *
 * @returns {JSX.Element} AppLayout wrapping the protected route content.
 */
export default function AuthGuard(): JSX.Element {
  const { isSignedIn } = useAsgardeo();
  const location = useLocation();
  const navigate = useNavigate();

  // After login, restore the saved deep link so it survives the Asgardeo SDK
  // reloading the page to `afterSignInUrl` ("/") after the callback (which would
  // otherwise drop us on the default landing). The key is consumed by
  // PostLoginRedirectConsumer once we arrive at the target — that consumer runs
  // above <Routes> so it also clears the key for routes AuthGuard never mounts
  // (e.g. the 404 page); clearing here would strand the key on a dead deep link
  // and bounce the next `/` visit back to it. The default `/` landing is
  // deferred to RootLanding in App.tsx while a redirect is pending.
  useEffect(() => {
    if (!isSignedIn) return;
    const redirect = sessionStorage.getItem(POST_LOGIN_REDIRECT_KEY);
    if (!redirect) return;
    // Compare (and restore) the full location including the hash, so anchor
    // permalinks like `/cases/:id#description` are honoured, not stripped.
    const here = location.pathname + location.search + location.hash;
    if (here !== redirect) {
      void navigate(redirect, { replace: true });
    }
  }, [isSignedIn, navigate, location.pathname, location.search, location.hash]);

  // Rendered once and reused as *both* ProtectedRoute's `loader` and its
  // `children` below. `useAsgardeo()`'s `isLoading` is not a one-shot signal —
  // the SDK polls it on an internal interval and it can flip true again well
  // after sign-in has settled (observed directly: isSignedIn stayed true while
  // isLoading flapped true/false every ~150ms). ProtectedRoute re-renders
  // `loader` or `children` based on that live value, so if the two were
  // different element trees (as they were previously: a bare <AppLayout/> vs.
  // <CurrentUserProvider><AppLayout/></CurrentUserProvider>), every flap made
  // React tear down and remount the whole subtree — including Header's avatar
  // (the repeated `commitMount`/429-on-Google-avatar loop) and AppLayout's own
  // `hasInitialized` latch (which reset to false on every remount, defeating
  // the very guard it exists to provide). Passing the *same* element for both
  // props makes a flap a no-op from React's reconciliation perspective: same
  // type, same position, no unmount.
  const protectedContent = (
    <CurrentUserProvider>
      {/* App-wide floating widget for the live-engineer-chat escalation
          feature — see EngineerAlertNotification's own doc comment. Mounted
          here (not inside AppLayout) so it survives route changes without
          remounting, the same reasoning IdleTimeoutProvider's
          SessionWarningDialog already follows one level up. */}
      <EngineerAlertNotification />
      <Outlet />
    </CurrentUserProvider>
  );

  return (
    <AppLayout>
      <ProtectedRoute
        loader={protectedContent}
        onSignIn={(defaultSignIn, signInOptions) => {
          const intended =
            location.pathname + location.search + location.hash;
          if (intended !== "/") {
            sessionStorage.setItem(POST_LOGIN_REDIRECT_KEY, intended);
          }
          defaultSignIn(signInOptions);
        }}
      >
        {protectedContent}
      </ProtectedRoute>
    </AppLayout>
  );
}
