<script lang="ts">
  import X from "phosphor-svelte/lib/XIcon";
  import { quoted, removeQuote } from "../state/quotes.svelte";

  let { field, disabled, onkeydown }: {
    field: HTMLElement | null;
    disabled: boolean;
    onkeydown: (event: KeyboardEvent) => void;
  } = $props();
  let list = $state<HTMLDivElement | null>(null);

  // Only a newly selected passage takes the caret.
  let seen = quoted.added;
  $effect(() => {
    if (quoted.added === seen) return;
    seen = quoted.added;
    list?.querySelector<HTMLTextAreaElement>(":scope > :last-child textarea")?.focus();
  });

  function discardQuote(index: number, focusIndex: number) {
    const next = list?.querySelectorAll<HTMLTextAreaElement>(".quote-reply")[focusIndex] ?? field;
    removeQuote(index);
    next?.focus();
  }

  function replyKey(event: KeyboardEvent, quoteIndex: number) {
    if (event.key === "Backspace" && !event.isComposing && !quoted.items[quoteIndex].reply) {
      event.preventDefault();
      if (!event.repeat) discardQuote(quoteIndex, quoteIndex - 1);
      return;
    }
    onkeydown(event);
  }
</script>

{#if quoted.items.length > 0}
  <div class="quotes mb-3" bind:this={list}>
    {#each quoted.items as q, i (q)}
      <div>
        <div class="quote">
          <span class="quote-text">{q.text}</span>
          <button
            class="quote-x"
            tabindex="-1"
            aria-label={`Remove quote ${i + 1}`}
            onclick={() => discardQuote(i, i + 1)}
          >
            <X size={12} />
          </button>
        </div>
        <textarea
          class="textarea quote-reply"
          rows="1"
          aria-label={`Reply to quote ${i + 1}`}
          placeholder="Reply to this quote"
          bind:value={q.reply}
          onkeydown={(event) => replyKey(event, i)}
          {disabled}
        ></textarea>
      </div>
    {/each}
  </div>
{/if}
