import type { QuoteDraft } from "../quote";

// Selected passages stay in the composer until the message is sent.
export const quoted = $state<{ items: QuoteDraft[]; added: number }>({ items: [], added: 0 });

export function addQuote(text: string): void {
  const clean = text.trim();
  if (!clean) return;
  quoted.items.push({ text: clean, reply: "" });
  quoted.added++;
}

export function removeQuote(index: number): void {
  quoted.items.splice(index, 1);
}

export function takeQuotes(): QuoteDraft[] {
  const items = quoted.items;
  quoted.items = [];
  return items;
}
