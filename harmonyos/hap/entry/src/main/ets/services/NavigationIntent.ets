// Keep explicit tray navigation across authentication/recovery loads until
// the requested page commits. Never replay an already acknowledged request.
export class NavigationIntent {
  private lastRequest: string = '';
  private pendingSettings: boolean = false;
  private retries: number = 0;

  update(destination: string): void {
    if (destination === this.lastRequest) return;
    this.lastRequest = destination;
    this.pendingSettings = destination.startsWith('settings:');
    this.retries = 0;
  }

  location(origin: string, current: string, preserve: boolean): string {
    if (this.pendingSettings) return origin + '/settings';
    return preserve && current.startsWith(origin + '/') ? current : origin + '/';
  }

  committed(origin: string, current: string): string {
    if (!this.pendingSettings || !current.startsWith(origin + '/')) return '';
    if (current === origin + '/settings' || current.startsWith(origin + '/settings?') ||
      current.startsWith(origin + '/settings#')) {
      this.pendingSettings = false;
      return '';
    }
    if (this.retries >= 3) return '';
    this.retries++;
    return origin + '/settings';
  }
}
