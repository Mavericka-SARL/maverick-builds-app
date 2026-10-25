import { useQuery } from "@tanstack/react-query";
import { api } from "./client";

/**
 * The demo context (the application, model and revision a console works in)
 * as the API client's selection resolves it, cached per selection and per
 * revision. One shared ["demo"] entry kept the previous application's model
 * after Developer › Models picked a revision of another for its 60 s: a
 * dashboard Preview sent that model with the new revision and every write
 * was refused, and Workflows listed nothing (2026-10-21, 2026-10-25). A
 * developer tab passes its working revision, and the server answers for
 * that revision's model.
 */
export function useDemo(revisionId?: string, options: { enabled?: boolean; staleTime?: number } = {}) {
  const appId = localStorage.getItem("selected_app_id") ?? "";
  const modelId = localStorage.getItem("selected_model_id") ?? "";
  return useQuery({
    queryKey: ["demo", appId, modelId, revisionId ?? ""],
    queryFn: () => api.getDemo(revisionId),
    ...options,
  });
}
