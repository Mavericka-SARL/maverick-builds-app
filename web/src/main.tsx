import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { AuthProvider } from "./auth/AuthProvider";
import { BrandingProvider } from "./branding/BrandingProvider";
import RoleRouter from "./router/RoleRouter";
import { SignUpPage } from "./signup/SignUpPage";
import "./index.css";

const queryClient = new QueryClient({
  defaultOptions: { queries: { staleTime: 30_000 } },
});

// /signup is the one page that exists before an account does: it renders
// outside the auth layer, which would otherwise send the visitor to the
// realm's login. Everything else is the console.
const isSignUp = window.location.pathname.replace(/\/+$/, "") === "/signup";

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <BrowserRouter>
      <QueryClientProvider client={queryClient}>
        <BrandingProvider>
          {isSignUp ? (
            <SignUpPage />
          ) : (
            <AuthProvider>
              <RoleRouter />
            </AuthProvider>
          )}
        </BrandingProvider>
      </QueryClientProvider>
    </BrowserRouter>
  </StrictMode>
);
