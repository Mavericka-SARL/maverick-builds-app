import { useQuery } from "@tanstack/react-query";
import { api, type FormDef } from "../../api/client";
import { keeperOf, rememberPeople } from "./recordList";

/**
 * What a table's record forms offer beyond its fields (GET
 * /api/forms/{id}/choices): the people each person field may name, every
 * named person's name, and the members a dimension field no longer offers.
 * Asked only of a table with a person field, or a dimension field whose
 * keeping table marks members active — any other table asks nothing.
 */
export function useRecordChoices(form: FormDef | undefined, forms: FormDef[] = []) {
  const needed = !!form && form.fields.some(f =>
    f.type === "person" || (f.type === "dimension" && !!keeperOf(forms, f)?.fields.some(x => x.active_flag)));
  const { data } = useQuery({
    queryKey: ["record-choices", form?.id],
    queryFn: async () => {
      const c = await api.recordChoices(form!.id);
      rememberPeople(c.named);
      return c;
    },
    enabled: needed,
    staleTime: 60_000,
  });
  return needed ? data : undefined;
}
