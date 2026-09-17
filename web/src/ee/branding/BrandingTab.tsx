import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, type Branding } from "../../api/client";
import { FeatureGate } from "../../license/FeatureGate";
import { brandTokens, useApplyBrand } from "../../branding/brand";
import { Button, Card, Field, InlineAlert, LoadingState, TextInput, useConfirm } from "../../ui";

const EMPTY: Branding = { product_name: "", tagline: "", logo_data_url: "", favicon_data_url: "", brand_color: "", email_from_name: "", custom_domain: "" };
const LOGO_LIMIT = 256 * 1024;
const FAVICON_LIMIT = 32 * 1024;

function readAsDataURL(file: File, limit: number): Promise<string> {
  return new Promise((resolve, reject) => {
    if (!file.type.startsWith("image/")) return reject(new Error("Choose an image file"));
    if (file.size > limit) return reject(new Error(`The image must be at most ${Math.round(limit / 1024)} KB`));
    const r = new FileReader();
    r.onload = () => resolve(String(r.result));
    r.onerror = () => reject(new Error("Could not read the file"));
    r.readAsDataURL(file);
  });
}

/**
 * Admin › Branding: the tenant's own name, tagline, logo, favicon and colour
 * on the console, the name on its e-mail, and the host whose visitors see
 * this brand before they sign in. Saved branding applies to this console at
 * once. Commercial and enterprise; other editions see the gate.
 */
export function BrandingTab() {
  return (
    <FeatureGate feature="white_label">
      <BrandingForm />
    </FeatureGate>
  );
}

function BrandingForm() {
  const qc = useQueryClient();
  const apply = useApplyBrand();
  const { confirm, confirmElement } = useConfirm();
  const { data, isLoading, error } = useQuery({ queryKey: ["branding"], queryFn: api.getAdminBranding });
  const [draft, setDraft] = useState<Branding | null>(null);
  const [fileError, setFileError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const form = draft ?? data ?? EMPTY;

  const onSettings = (next: Branding) => {
    qc.setQueryData(["branding"], next);
    setDraft(null);
    apply({ product_name: next.product_name, tagline: next.tagline, logo_data_url: next.logo_data_url, favicon_data_url: next.favicon_data_url, brand_color: next.brand_color, configured: !!next.configured, source: "tenant" });
  };
  const save = useMutation({ mutationFn: () => api.updateBranding(form), onSuccess: (n) => { onSettings(n); setSaved(true); setTimeout(() => setSaved(false), 2000); } });
  const remove = useMutation({ mutationFn: api.removeBranding, onSuccess: onSettings });

  if (isLoading) return <LoadingState label="Loading branding…" />;
  if (error) return <InlineAlert tone="danger">{(error as Error).message}</InlineAlert>;

  const set = (patch: Partial<Branding>) => setDraft({ ...form, ...patch });
  const pick = (key: "logo_data_url" | "favicon_data_url", limit: number) => async (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (!file) return;
    setFileError(null);
    try { set({ [key]: await readAsDataURL(file, limit) }); } catch (err) { setFileError((err as Error).message); }
  };
  const tokens = form.brand_color ? brandTokens(form.brand_color) : null;
  const removeBrand = () => confirm({
    title: "Remove the branding?",
    body: "The console returns to the platform's own name, logo and colours for everyone in this tenant.",
    confirmLabel: "Remove branding", destructive: true, onConfirm: () => remove.mutate(),
  });

  return (
    <div className="mvx-admin-stack" data-testid="branding">
      <Card>
        <div style={{ fontWeight: 700, marginBottom: 4 }}>Name and colour</div>
        <div style={{ display: "grid", gap: 12, maxWidth: 560 }}>
          <Field label="Product name" description="Shown in the sidebar and the browser tab">
            <TextInput value={form.product_name} onChange={(e) => set({ product_name: e.target.value })} placeholder="Acme Planning" aria-label="Product name" maxLength={60} />
          </Field>
          <Field label="Tagline" description="Under the name on the sign-in page">
            <TextInput value={form.tagline} onChange={(e) => set({ tagline: e.target.value })} aria-label="Tagline" maxLength={120} />
          </Field>
          <Field label="Brand colour" description="Buttons, links and highlights; two darker and three lighter steps are derived from it">
            <div style={{ display: "flex", gap: 8, alignItems: "center" }}>
              <input type="color" value={form.brand_color || "#4f46e5"} onChange={(e) => set({ brand_color: e.target.value })} aria-label="Brand colour picker" />
              <TextInput value={form.brand_color} onChange={(e) => set({ brand_color: e.target.value })} placeholder="#4f46e5" aria-label="Brand colour" style={{ width: 120 }} />
              {tokens && <span data-testid="brand-swatches" style={{ display: "flex", gap: 4 }}>{Object.values(tokens).map((c) => <span key={c} title={c} style={{ width: 18, height: 18, borderRadius: 4, background: c, border: "1px solid var(--color-border)" }} />)}</span>}
            </div>
          </Field>
        </div>
      </Card>
      <Card>
        <div style={{ fontWeight: 700, marginBottom: 4 }}>Logo and favicon</div>
        <p className="mvx-admin-muted" style={{ marginTop: 0 }}>PNG, JPEG, SVG or WebP. Logo up to 256 KB, favicon up to 32 KB.</p>
        <div style={{ display: "flex", gap: 24, flexWrap: "wrap" }}>
          <Field label="Logo">
            <input type="file" accept="image/*" onChange={pick("logo_data_url", LOGO_LIMIT)} aria-label="Logo file" />
            {form.logo_data_url && <div style={{ marginTop: 8 }}><img src={form.logo_data_url} alt="Logo preview" style={{ height: 40 }} data-testid="logo-preview" /> <Button size="sm" variant="ghost" onClick={() => set({ logo_data_url: "" })}>Remove</Button></div>}
          </Field>
          <Field label="Favicon">
            <input type="file" accept="image/*" onChange={pick("favicon_data_url", FAVICON_LIMIT)} aria-label="Favicon file" />
            {form.favicon_data_url && <div style={{ marginTop: 8 }}><img src={form.favicon_data_url} alt="Favicon preview" style={{ height: 16 }} /> <Button size="sm" variant="ghost" onClick={() => set({ favicon_data_url: "" })}>Remove</Button></div>}
          </Field>
        </div>
        {fileError && <InlineAlert tone="danger">{fileError}</InlineAlert>}
      </Card>
      <Card>
        <div style={{ fontWeight: 700, marginBottom: 4 }}>E-mail and domain</div>
        <div style={{ display: "grid", gap: 12, maxWidth: 560 }}>
          <Field label="E-mail sender name" description="Notifications arrive from this name; the address stays the platform's relay">
            <TextInput value={form.email_from_name} onChange={(e) => set({ email_from_name: e.target.value })} placeholder={form.product_name || "Acme Planning"} aria-label="E-mail sender name" maxLength={60} />
          </Field>
          <Field label="Custom domain" description="Visitors of this host see your brand before they sign in. Pointing the host at this platform (DNS, certificate) is done by whoever runs it.">
            <TextInput value={form.custom_domain} onChange={(e) => set({ custom_domain: e.target.value })} placeholder="planning.acme.com" aria-label="Custom domain" />
          </Field>
        </div>
      </Card>
      <div className="mvx-admin-inline-form">
        <Button variant="primary" loading={save.isPending} loadingLabel="Saving…" onClick={() => save.mutate()}>{saved ? "Saved" : "Save branding"}</Button>
        {data?.configured && <Button variant="danger" loading={remove.isPending} loadingLabel="Removing…" onClick={removeBrand}>Remove branding</Button>}
        {save.isError && <span className="mvx-admin-error">{(save.error as Error).message}</span>}
      </div>
      {confirmElement}
    </div>
  );
}
