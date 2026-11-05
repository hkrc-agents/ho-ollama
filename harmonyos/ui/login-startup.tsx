import { useEffect, useState } from "react";
import { Button } from "@/components/ui/button";
import { Field, Label, Description } from "@/components/ui/fieldset";

const labels: Record<string, string> = {
  unknown: "Checking…",
  enabled: "Enabled",
  disabled: "Disabled",
  unsupported: "Not supported on this system",
  error: "Unable to read system setting",
};

export function LoginStartup() {
  const [status, setStatus] = useState("unknown");
  useEffect(() => {
    const receive = (event: Event) => {
      const value = (event as CustomEvent).detail?.status;
      if (typeof value === "string" && Object.prototype.hasOwnProperty.call(labels, value)) setStatus(value);
    };
    const refresh = () => {
      if (!document.hidden) window.location.href = "harmony-ollama://login-startup-status";
    };
    window.addEventListener("harmony-login-startup-state", receive);
    window.addEventListener("focus", refresh);
    document.addEventListener("visibilitychange", refresh);
    refresh();
    const initialTimeout = window.setTimeout(() => setStatus(value => value === "unknown" ? "error" : value), 4000);
    const timer = window.setInterval(refresh, 5000);
    return () => {
      window.clearTimeout(initialTimeout);
      window.clearInterval(timer);
      window.removeEventListener("harmony-login-startup-state", receive);
      window.removeEventListener("focus", refresh);
      document.removeEventListener("visibilitychange", refresh);
    };
  }, []);
  return (
    <div className="overflow-hidden rounded-xl bg-white dark:bg-neutral-800 p-4">
      <Field>
        <div className="flex items-center justify-between gap-4">
          <Label className="min-w-0">Start Ollama at login</Label>
          <span role="status" aria-live="polite" className="ml-auto text-right text-sm text-neutral-500 dark:text-neutral-400">
            System status: {labels[status]}
          </span>
        </div>
        <Description>Managed by HarmonyOS: Settings → Apps &amp; services → Launch manager.</Description>
        <Button type="button" color="white"
          onClick={() => { window.location.href = "harmony-ollama://login-startup"; }}>
          Manage login startup
        </Button>
      </Field>
    </div>
  );
}

export function HarmonyNotificationSettings() {
  const [status, setStatus] = useState("unknown");
  const [banners, setBanners] = useState("unknown");
  useEffect(() => {
    const receive = (event: Event) => {
      const detail = (event as CustomEvent).detail;
      if (detail && typeof detail.notifications === "string" && Object.prototype.hasOwnProperty.call(labels, detail.notifications)) setStatus(detail.notifications);
      if (detail && typeof detail.serviceBanners === "string" && Object.prototype.hasOwnProperty.call(labels, detail.serviceBanners)) setBanners(detail.serviceBanners);
    };
    let pending: number | undefined;
    const refresh = () => {
      window.clearTimeout(pending);
      pending = window.setTimeout(() => {
        if (!document.hidden) window.location.href = "harmony-ollama://notification-status";
      }, 250); // Do not replace the login-status navigation in the same event turn.
    };
    window.addEventListener("harmony-notification-state", receive);
    window.addEventListener("focus", refresh);
    document.addEventListener("visibilitychange", refresh);
    refresh();
    const timeout = window.setTimeout(() => {
      setStatus(value => value === "unknown" ? "error" : value);
    }, 4000);
    const timer = window.setInterval(refresh, 5000);
    return () => {
      window.clearTimeout(timeout); window.clearInterval(timer);
      window.clearTimeout(pending);
      window.removeEventListener("harmony-notification-state", receive);
      window.removeEventListener("focus", refresh);
      document.removeEventListener("visibilitychange", refresh);
    };
  }, []);
  return (
    <div className="overflow-hidden rounded-xl bg-white dark:bg-neutral-800 p-4">
      <Field>
        <div className="flex items-center justify-between gap-4">
          <Label className="min-w-0">Background notifications</Label>
          <span role="status" aria-live="polite" className="ml-auto text-right text-sm text-neutral-500 dark:text-neutral-400">
            Notifications: {labels[status]}
          </span>
        </div>
        <Description>Enable Allow notifications and Banners in HarmonyOS to see the close-to-tray reminder.</Description>
        <Description>Service channel banners: {banners === "unknown" ? "Not available" : labels[banners]}. The system-wide banner switch and Do Not Disturb must be checked in system settings; they cannot be read here.</Description>
        <Button type="button" color="white" onClick={() => { window.location.href = "harmony-ollama://notification-settings"; }}>
          Manage notifications
        </Button>
      </Field>
    </div>
  );
}

export function HarmonyLoginStartup() {
  return <><LoginStartup /><HarmonyNotificationSettings /></>;
}
