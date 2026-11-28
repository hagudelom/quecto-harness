// Quotes, replies and the final message share one Markdown string in the log.
export interface QuoteDraft {
  text: string;
  reply: string;
}

type Segment = { kind: "quote" | "text" | "break"; text: string };
const quoteMarker = /^>(?:[ \t]|$)/;

function nextFence(line: string, open: string): string {
  const marker = /^ {0,3}(`{3,}|~{3,})(.*)$/.exec(line);
  if (!marker) return open;
  const [, run, tail] = marker;
  if (open) return run.startsWith(open) && !tail.trim() ? "" : open;
  return run.startsWith("~") || !tail.includes("`") ? run : "";
}

function replyBlock(text: string, closeFence = true): string {
  let fence = "";
  const lines = text.trim().split("\n").map((line) => {
    // Before a literal greater-than sign, Markdown needs two backslashes
    // per literal backslash and one to escape the sign itself.
    const result = fence ? line : line.replace(/^(\\*)>/, "$1$1\\>");
    fence = nextFence(line, fence);
    return result;
  });
  if (closeFence && fence) lines.push(fence);
  return lines.join("\n");
}

/** Each selected passage is followed by its reply, then the general message. */
export function withQuotes(quotes: QuoteDraft[], text: string): string {
  const blocks: string[] = [];
  for (const quote of quotes) {
    const block = quote.text.split("\n").map((line) => (line.trim() ? `> ${line}` : ">")).join("\n");
    blocks.push(block, replyBlock(quote.reply));
  }
  const hasReply = quotes.some((quote) => quote.reply.trim());
  if (hasReply && text.trim()) blocks.push("---");
  blocks.push(hasReply ? replyBlock(text, false) : text.trim());
  return blocks.filter(Boolean).join("\n\n");
}

/** Only an opening blockquote introduces quoted passages in the bubble. */
export function splitQuotes(text: string): Segment[] {
  if (!quoteMarker.test(text.split("\n", 1)[0])) {
    const body = text.trim();
    return body ? [{ kind: "text", text: body }] : [];
  }

  const segments: Segment[] = [];
  let kind: Segment["kind"] = "quote";
  let lines: string[] = [];
  let fence = "";

  function flush() {
    const content = lines.join("\n");
    if (kind === "text" && content === "---") {
      segments.push({ kind: "break", text: content });
    } else if (content.trim()) {
      segments.push({ kind, text: kind === "quote" ? content.trim() : content });
    }
    lines = [];
  }

  for (const line of text.trimEnd().split("\n")) {
    const isQuote: boolean = !fence && (kind === "quote" || !lines.length) && quoteMarker.test(line);
    const next: Segment["kind"] = isQuote ? "quote" : "text";
    if (next !== kind || (!fence && !line.trim())) {
      flush();
      kind = next;
    }
    if (isQuote) {
      lines.push(line.replace(quoteMarker, ""));
    } else if (fence || line.trim()) {
      const content = fence ? line : line.replace(/^\\((?:\\\\)*)>/, (_, pairs: string) =>
        pairs.slice(0, pairs.length / 2) + ">");
      lines.push(content);
      fence = nextFence(line, fence);
    }
  }
  flush();
  return segments;
}
