import { useState } from "react";
import { Bell } from "lucide-react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { api, type Notification } from "../api/client";
import { IconButton } from "./IconButton";
import { Drawer } from "./Drawer";
import { Badge } from "./Badge";
import { LoadingState } from "./LoadingState";
import { EmptyState } from "./EmptyState";

interface NotificationCenterProps {
  /** Called when the caller clicks a notification that has a resource_type/resource_id — decides what "navigate" means for the current console. */
  onNavigate?: (resourceType: string, resourceId: string) => void;
}

export function NotificationCenter({ onNavigate }: NotificationCenterProps) {
  const [open, setOpen] = useState(false);
  const qc = useQueryClient();

  const { data: notifications = [], isLoading } = useQuery({
    queryKey: ["notifications"],
    queryFn: api.getNotifications,
    refetchInterval: 10_000, // mirrors WorkflowInbox's existing poll interval
  });

  const unreadCount = notifications.filter((n) => n.status !== "read").length;

  const markRead = useMutation({
    mutationFn: (ids: string[]) => api.markRead(ids),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["notifications"] }),
  });

  function handleClick(n: Notification) {
    if (n.status !== "read") markRead.mutate([n.id]);
    if (n.resource_type && n.resource_id) onNavigate?.(n.resource_type, n.resource_id);
    setOpen(false);
  }

  return (
    <>
      <div style={{ position: "relative", display: "inline-flex" }}>
        <IconButton
          aria-label={unreadCount > 0 ? `Notifications, ${unreadCount} unread` : "Notifications"}
          onClick={() => setOpen(true)}
        >
          <Bell size={16} />
        </IconButton>
        {unreadCount > 0 && (
          <Badge
            color="danger"
            style={{
              position: "absolute",
              top: -4,
              right: -4,
              fontSize: 10,
              lineHeight: "14px",
              padding: "0 5px",
              pointerEvents: "none",
            }}
          >
            {unreadCount}
          </Badge>
        )}
      </div>
      <Drawer open={open} onClose={() => setOpen(false)} title="Notifications">
        {isLoading ? (
          <LoadingState label="Loading notifications…" />
        ) : notifications.length === 0 ? (
          <EmptyState label="No notifications yet." />
        ) : (
          notifications.map((n) => {
            const unread = n.status !== "read";
            const navigable = Boolean(n.resource_type && n.resource_id);
            const vars = n.template_vars ?? {};
            return (
              <div
                key={n.id}
                onClick={() => handleClick(n)}
                role="button"
                tabIndex={0}
                onKeyDown={(e) => { if (e.key === "Enter") handleClick(n); }}
                style={{
                  cursor: navigable || unread ? "pointer" : "default",
                  padding: "10px 0",
                  borderBottom: "1px solid var(--color-border)",
                  opacity: unread ? 1 : 0.6,
                }}
              >
                <div style={{ fontWeight: unread ? 600 : 400, fontSize: 13 }}>
                  {vars.subject || n.template_id}
                </div>
                {vars.message && (
                  <div className="mvx-admin-muted" style={{ fontSize: 12, marginTop: 2 }}>
                    {vars.message}
                  </div>
                )}
                <div className="mvx-admin-muted" style={{ fontSize: 11, marginTop: 4 }}>
                  {new Date(n.created_at).toLocaleString()}
                </div>
              </div>
            );
          })
        )}
      </Drawer>
    </>
  );
}
