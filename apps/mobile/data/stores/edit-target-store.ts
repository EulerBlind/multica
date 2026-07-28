/**
 * Screen-scoped state for "edit comment X" — written by the comment
 * long-press action sheet ("Edit"), read by InlineCommentComposer.
 *
 * Mirrors `reply-target-store.ts`: trigger and consumer live in different
 * trees, and only one edit can be in-flight per issue screen.
 *
 * Mutually exclusive with reply: setting an edit target should clear any
 * reply target (and vice versa) so the composer chip stays unambiguous.
 */
import { create } from "zustand";

export interface EditTarget {
  commentId: string;
  /** Raw markdown currently saved on the comment — seeds the composer. */
  content: string;
  /** Attachment ids currently linked to the comment (optional swap on save). */
  attachmentIds: string[];
}

interface State {
  target: EditTarget | null;
  setTarget: (target: EditTarget) => void;
  clear: () => void;
}

export const useEditTargetStore = create<State>((set) => ({
  target: null,
  setTarget: (target) => set({ target }),
  clear: () => set({ target: null }),
}));
