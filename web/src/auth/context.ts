import { createContext } from "react";

export interface AuthContextValue {
  authenticated: boolean;
  token: string | undefined;
  userRoles: string[];
  persona: string;
  setPersona: (p: string) => void;
  logout: () => void;
}

export const AuthContext = createContext<AuthContextValue | null>(null);
