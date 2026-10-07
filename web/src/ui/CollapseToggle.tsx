import { ChevronDown, ChevronRight } from "lucide-react";
import { IconButton } from "./IconButton";

/** The chevron that collapses or expands a row's children (see useCollapsed). */
export function CollapseToggle({ expanded, onToggle, label }: { expanded: boolean; onToggle: () => void; label: string }) {
  return (
    <IconButton
      aria-label={`${expanded ? "Collapse" : "Expand"} ${label}`}
      aria-expanded={expanded}
      title={expanded ? "Collapse" : "Expand"}
      size={26}
      onClick={onToggle}
    >
      {expanded ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
    </IconButton>
  );
}
