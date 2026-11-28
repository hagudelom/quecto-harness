import assert from "node:assert/strict";
import test from "node:test";
import { splitQuotes, withQuotes } from "../src/lib/quote.ts";

test("Unanswered quotes keep the original message bytes", () => {
  const drafts = [
    { text: "First\n \n  Detail", reply: "" },
    { text: "Second", reply: " \t\n" },
  ];
  const prefix = "> First\n>\n>   Detail\n\n> Second";
  assert.equal(withQuotes(drafts, "  General\ntext  "), prefix + "\n\nGeneral\ntext");
  assert.equal(withQuotes(drafts, ""), prefix);
  assert.equal(withQuotes([], "  General  "), "General");
  assert.equal(withQuotes([], " "), "");
  assert.deepEqual(splitQuotes(prefix + "\n\nGeneral\ntext"), [
    { kind: "quote", text: "First\n\n  Detail" },
    { kind: "quote", text: "Second" },
    { kind: "text", text: "General\ntext" },
  ]);
});

test("Quotes and replies round trip as interleaved Markdown", () => {
  const message = withQuotes([
    { text: "First", reply: " Answer one\ncontinued " },
    { text: "Second", reply: "" },
    { text: "Third", reply: "Answer three" },
  ], "General");
  assert.equal(message, "> First\n\nAnswer one\ncontinued\n\n> Second\n\n> Third\n\nAnswer three\n\n---\n\nGeneral");
  assert.deepEqual(splitQuotes(message), [
    { kind: "quote", text: "First" },
    { kind: "text", text: "Answer one\ncontinued" },
    { kind: "quote", text: "Second" },
    { kind: "quote", text: "Third" },
    { kind: "text", text: "Answer three" },
    { kind: "break", text: "---" },
    { kind: "text", text: "General" },
  ]);
});

test("Messages without an opening quote keep their entire body", () => {
  for (const body of ["Here is the log:\n> error", ">= 3 workers", "Sender\n\n> copied", " \n> Later"]) {
    assert.deepEqual(splitQuotes(body), [{ kind: "text", text: body.trim() }]);
  }
});

test("A log inside a paragraph keeps its greater-than signs", () => {
  assert.deepEqual(splitQuotes("> Quote\n\nHere is the log:\n> error"), [
    { kind: "quote", text: "Quote" },
    { kind: "text", text: "Here is the log:\n> error" },
  ]);
});

test("An inline code span at the start of a reply leaves the next quote visible", () => {
  assert.deepEqual(splitQuotes("> First\n\n```inline```\n\n> Second"), [
    { kind: "quote", text: "First" },
    { kind: "text", text: "```inline```" },
    { kind: "quote", text: "Second" },
  ]);
});

test("Literal greater-than signs and backslashes round trip in replies and general text", () => {
  const reply = "> literal\n>= comparison\n\\> escaped\n\\\\> doubled";
  const message = withQuotes([{ text: "Question", reply }], "> General");
  assert.equal(message, "> Question\n\n\\> literal\n\\>= comparison\n\\\\\\> escaped\n\\\\\\\\\\> doubled\n\n---\n\n\\> General");
  assert.deepEqual(splitQuotes(message), [
    { kind: "quote", text: "Question" },
    { kind: "text", text: reply },
    { kind: "break", text: "---" },
    { kind: "text", text: "> General" },
  ]);
  assert.equal(withQuotes([{ text: "Question", reply: "" }], "> General"), "> Question\n\n> General");
});

test("A missing closing fence is supplied before the next passage", () => {
  for (const fence of ["````", "~~~~"]) {
    const reply = fence + "sh\n\n> output\n" + fence.slice(1) + "\n\\> raw";
    const drafts = [{ text: "First", reply }, { text: "Next", reply: "Done" }];
    assert.equal(withQuotes(drafts, ""), "> First\n\n" + reply + "\n" + fence + "\n\n> Next\n\nDone");
    assert.deepEqual(splitQuotes(withQuotes(drafts, "")), [
      { kind: "quote", text: "First" },
      { kind: "text", text: reply + "\n" + fence },
      { kind: "quote", text: "Next" },
      { kind: "text", text: "Done" },
    ]);
    drafts[0].reply += "\n" + fence;
    assert.equal(withQuotes(drafts, ""), "> First\n\n" + drafts[0].reply + "\n\n> Next\n\nDone");
  }
});

test("The general message follows a break even if the last quote has no reply", () => {
  const drafts = [{ text: "First", reply: "Answer" }, { text: "Last", reply: "" }];
  const prefix = "> First\n\nAnswer\n\n> Last";
  assert.equal(withQuotes(drafts, " \t\n"), prefix);
  const message = withQuotes(drafts, "  ok  ");
  assert.equal(message, prefix + "\n\n---\n\nok");
  assert.deepEqual(splitQuotes(message).slice(-3), [
    { kind: "quote", text: "Last" },
    { kind: "break", text: "---" },
    { kind: "text", text: "ok" },
  ]);
});

test("A thematic break keeps its meaning whoever inserted it", () => {
  const mixed = withQuotes([{ text: "Passage", reply: "Answer" }], "ok");
  const authored = withQuotes([{ text: "Passage", reply: "Answer\n\n---\n\nok" }], "");
  assert.equal(authored, mixed);
  assert.deepEqual(splitQuotes(authored), [
    { kind: "quote", text: "Passage" },
    { kind: "text", text: "Answer" },
    { kind: "break", text: "---" },
    { kind: "text", text: "ok" },
  ]);
});

test("A break preserves dashes in quotes, headings, code and general text", () => {
  const code = "```yaml\n\n---\n\nkey: value\n```";
  const reply = "Heading\n---\n\n" + code;
  const general = "First paragraph\n\n---\n\nLast paragraph";
  const message = withQuotes([{ text: "---", reply }], general);
  assert.equal(message, "> ---\n\n" + reply + "\n\n---\n\n" + general);
  assert.deepEqual(splitQuotes(message), [
    { kind: "quote", text: "---" },
    { kind: "text", text: "Heading\n---" },
    { kind: "text", text: code },
    { kind: "break", text: "---" },
    { kind: "text", text: "First paragraph" },
    { kind: "break", text: "---" },
    { kind: "text", text: "Last paragraph" },
  ]);
  assert.deepEqual(splitQuotes("---"), [{ kind: "text", text: "---" }]);
});
