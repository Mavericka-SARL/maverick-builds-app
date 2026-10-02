import { useQuery } from "@tanstack/react-query";
import { api } from "../api/client";

/** The chat connector's connection details (GET /api/connector). */
export function useConnectorInfo() {
  return useQuery({ queryKey: ["connector-info"], queryFn: () => api.getConnectorInfo(), staleTime: 5 * 60_000 });
}
