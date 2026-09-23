import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { AuthProvider } from "./auth/AuthProvider";
import { BrandingProvider } from "./branding/BrandingProvider";
import RoleRouter from "./router/RoleRouter";
import { SignUpPage } from "./signup/SignUpPage";
import { LegalPage } from "./public/legal/LegalPage";
import "./index.css";

const queryClient = new QueryClient({
  defaultOptions: { queries: { staleTime: 30_000 } },
});

// The pages that exist before an account does: sign-up, and the two documents
// it asks the visitor to agree to. They render outside the auth layer, which
// would otherwise send the visitor to the realm's login — and requiring an
// account to read the terms of the account would be absurd. Everything else is
// the console.
const publicPath = window.location.pathname.replace(/\/+$/, "");
const publicPage =
  publicPath === "/signup" ? <SignUpPage /> :
  publicPath === "/terms" ? <LegalPage doc="terms" /> :
  publicPath === "/privacy" ? <LegalPage doc="privacy" /> :
  null;

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <BrowserRouter>
      <QueryClientProvider client={queryClient}>
        <BrandingProvider>
          {publicPage ?? (
            <AuthProvider>
              <RoleRouter />
            </AuthProvider>
          )}
        </BrandingProvider>
      </QueryClientProvider>
    </BrowserRouter>
  </StrictMode>
);
