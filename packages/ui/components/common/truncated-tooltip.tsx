"use client";

import { useRef, useState, type ComponentProps, type ReactElement, type ReactNode } from "react";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@multica/ui/components/ui/tooltip";

/**
 * Whether `node` hides part of its content. Overflow only counts on an axis
 * that actually clips (`overflow` other than `visible`): an emoji icon that
 * spills out of its fixed-size box is still fully visible, so it is not
 * truncation. Covers `truncate` (horizontal) and `line-clamp-*` (vertical).
 */
const CLIPPING_OVERFLOW = new Set(["hidden", "clip", "auto", "scroll"]);

function clipsContent(node: Element): boolean {
  const overflowX = node.scrollWidth > node.clientWidth;
  const overflowY = node.scrollHeight > node.clientHeight;
  if (!overflowX && !overflowY) return false;
  const style = getComputedStyle(node);
  return (
    (overflowX && CLIPPING_OVERFLOW.has(style.overflowX)) ||
    (overflowY && CLIPPING_OVERFLOW.has(style.overflowY))
  );
}

/** True when `el` or any element inside it cuts off part of its content. */
export function isContentTruncated(el: HTMLElement | null): boolean {
  if (!el) return false;
  if (clipsContent(el)) return true;
  for (const node of el.querySelectorAll("*")) {
    if (clipsContent(node)) return true;
  }
  return false;
}

/**
 * Tooltip that reveals the full text only when the trigger's text is actually
 * cut off. Built on the shared `Tooltip` (same styling and open delay); right
 * before it would open, it measures the trigger and its descendants and stays
 * closed when nothing overflows. Measuring at open time keeps it correct
 * across resizes and content changes without observers.
 *
 * Use it for tooltips whose content repeats the trigger's own text. Tooltips
 * that add information (icon labels, disabled reasons) keep using `Tooltip`.
 */
function TruncatedTooltip({
  content,
  side = "top",
  children,
}: {
  /** Full text shown when the trigger is truncated. Empty renders the child alone. */
  content: ReactNode;
  side?: ComponentProps<typeof TooltipContent>["side"];
  /** The trigger element. Its own props and element type are preserved. */
  children: ReactElement;
}) {
  const triggerRef = useRef<HTMLButtonElement | null>(null);
  const [open, setOpen] = useState(false);

  if (content == null || content === false || content === "") return children;

  return (
    <Tooltip
      open={open}
      onOpenChange={(next) => {
        if (next && !isContentTruncated(triggerRef.current)) return;
        setOpen(next);
      }}
    >
      <TooltipTrigger ref={triggerRef} render={children} />
      <TooltipContent side={side}>{content}</TooltipContent>
    </Tooltip>
  );
}

export { TruncatedTooltip };
