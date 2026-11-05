export const run: (root: string, id: string, argv: string[], input: string, timeout: number, interactive?: boolean) => string;
export const cancel: (id: string) => string;
export const sendInput: (id: string, text: string, eof: boolean) => string;
export const snapshot: (id: string) => string;
