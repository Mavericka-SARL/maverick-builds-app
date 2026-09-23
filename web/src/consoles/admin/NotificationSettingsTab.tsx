import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, type NotificationSettings } from "../../api/client";
import { Button, Card, Checkbox, Field, InlineAlert, LoadingState, NumberInput, TextInput } from "../../ui";
import { SettingsScopeNotice } from "./SettingsScopeNotice";

const EMPTY: NotificationSettings = {
  email_enabled: false,
  webhook_enabled: false,
  webhook_url: "",
  reminders_enabled: false,
  reminder_lead_hours: 0,
};

/**
 * Admin › Notifications: which channels this tenant delivers on.
 *
 * Every notification is always written to the console. These switches decide
 * whether it also leaves the platform — by e-mail to the recipient, or as a
 * signed webhook — and whether an overdue task reminds the people who can act
 * on it. The SMTP relay itself is deployment configuration and is not editable
 * here; the banner says whether one exists, because turning e-mail on without
 * a relay only queues notifications that cannot be delivered.
 */
export function NotificationSettingsTab() {
  const qc = useQueryClient();
  const { data, isLoading, error } = useQuery({ queryKey: ["notification-settings"], queryFn: api.getNotificationSettings });
  // The saved settings are the source of truth; a draft only exists once
  // something has been edited, so no effect has to copy one into the other.
  const [draft, setDraft] = useState<NotificationSettings | null>(null);
  const [secret, setSecret] = useState("");
  const [saved, setSaved] = useState(false);
  const form = draft ?? data ?? EMPTY;

  const save = useMutation({
    mutationFn: () => api.updateNotificationSettings({ ...form, webhook_secret: secret || undefined }),
    onSuccess: (next) => {
      qc.setQueryData(["notification-settings"], next);
      setDraft(null);
      setSecret("");
      setSaved(true);
      setTimeout(() => setSaved(false), 2000);
    },
  });

  // The relay is proven by sending, not by reading its configuration back:
  // the message goes to the signed-in administrator's own address, and the
  // relay's own answer (accepted, refused, no answer) is shown as it came.
  const testSend = useMutation({ mutationFn: api.sendNotificationTestMail });

  // A tenant with settings of its own can drop them and follow the
  // deployment's defaults again (enterprise deployment_settings).
  const inherit = useMutation({
    mutationFn: api.clearNotificationSettings,
    onSuccess: (next) => {
      qc.setQueryData(["notification-settings"], next);
      setDraft(null);
    },
  });

  if (isLoading) return <LoadingState label="Loading notification settings…" />;
  if (error) return <InlineAlert tone="danger">{(error as Error).message}</InlineAlert>;

  const set = (patch: Partial<NotificationSettings>) => setDraft({ ...form, ...patch });
  const scope = data?.scope;

  return (
    <div className="mvx-admin-stack" data-testid="notification-settings">
      <SettingsScopeNotice scope={scope} onInherit={() => inherit.mutate()} inheriting={inherit.isPending} />
      <Card>
        <div style={{ fontWeight: 700, marginBottom: 4 }}>E-mail</div>
        <p className="mvx-admin-muted" style={{ marginTop: 0 }}>
          Send every notification to the recipient&apos;s address as well as to their notification centre.
        </p>
        {data?.mailer_configured === false && (
          <InlineAlert tone="warning">
            This deployment has no mail relay configured (<code>SMTP_HOST</code>), so e-mail notifications cannot be delivered
            until an operator sets one up.
          </InlineAlert>
        )}
        <Checkbox
          checked={form.email_enabled}
          onChange={(e) => set({ email_enabled: e.target.checked })}
          label="Send notifications by e-mail"
        />
        {data?.mailer_configured && (
          <div style={{ display: "flex", alignItems: "center", gap: 12, marginTop: 12, flexWrap: "wrap" }}>
            <Button
              variant="secondary"
              onClick={() => testSend.mutate()}
              disabled={testSend.isPending}
              data-testid="send-test-mail"
            >
              {testSend.isPending ? "Sending…" : "Send me a test e-mail"}
            </Button>
            <span className="mvx-admin-muted">Goes to your own address only, through this deployment&apos;s relay.</span>
          </div>
        )}
        {testSend.isSuccess && (
          <InlineAlert tone="success">
            The relay accepted a test message for <strong>{testSend.data.sent_to}</strong>. If it does not arrive, the problem is
            after the relay: the sender address, or the recipient&apos;s filters.
          </InlineAlert>
        )}
        {testSend.isError && <InlineAlert tone="danger">{(testSend.error as Error).message}</InlineAlert>}
      </Card>

      <Card>
        <div style={{ fontWeight: 700, marginBottom: 4 }}>Webhook</div>
        <p className="mvx-admin-muted" style={{ marginTop: 0 }}>
          Post every notification as JSON to a URL of yours. Each delivery carries an
          <code> X-Mavericks-Signature</code> header so the receiver can verify it came from here.
        </p>
        <Checkbox
          checked={form.webhook_enabled}
          onChange={(e) => set({ webhook_enabled: e.target.checked })}
          label="Send notifications to a webhook"
        />
        {form.webhook_enabled && (
          <div style={{ display: "grid", gap: 12, marginTop: 12, maxWidth: 520 }}>
            <Field label="Endpoint URL">
              <TextInput
                value={form.webhook_url}
                onChange={(e) => set({ webhook_url: e.target.value })}
                placeholder="https://hooks.example.com/mavericks"
                aria-label="Webhook URL"
              />
            </Field>
            <Field
              label="Signing secret"
              description={data?.has_webhook_secret ? "A secret is stored; leave blank to keep it" : "Optional, but recommended"}
            >
              <TextInput
                type="password"
                value={secret}
                onChange={(e) => setSecret(e.target.value)}
                placeholder={data?.has_webhook_secret ? "••••••••" : "shared secret"}
                aria-label="Webhook signing secret"
              />
            </Field>
          </div>
        )}
      </Card>

      <Card>
        <div style={{ fontWeight: 700, marginBottom: 4 }}>Task reminders</div>
        <p className="mvx-admin-muted" style={{ marginTop: 0 }}>
          When a workflow step reaches the due time its designer set with SLA hours, remind everyone who can act on it —
          once per task, through the channels above.
        </p>
        <Checkbox
          checked={form.reminders_enabled}
          onChange={(e) => set({ reminders_enabled: e.target.checked })}
          label="Remind assignees about tasks that come due"
        />
        {form.reminders_enabled && (
          <div style={{ marginTop: 12, maxWidth: 260 }}>
            <Field label="Remind this many hours before it is due" description="0 reminds at the due time">
              <NumberInput
                min={0}
                max={168}
                value={form.reminder_lead_hours}
                onChange={(e) => set({ reminder_lead_hours: Number(e.target.value) || 0 })}
                aria-label="Reminder lead hours"
              />
            </Field>
          </div>
        )}
      </Card>

      <div className="mvx-admin-inline-form">
        <Button variant="primary" loading={save.isPending} loadingLabel="Saving…" onClick={() => save.mutate()}>
          {saved ? "Saved" : "Save settings"}
        </Button>
        {save.isError && <span className="mvx-admin-error">{(save.error as Error).message}</span>}
      </div>
    </div>
  );
}
