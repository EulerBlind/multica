/**
 * Inline issue-comment composer — thin wrapper around the shared
 * `<MessageComposer>` with comment-specific wiring:
 *
 *   - Create → `useCreateComment(issueId).mutateAsync`
 *   - Edit   → `useEditComment(issueId).mutateAsync` when edit-target store
 *              is set (long-press → Edit on own comments)
 *   - Reply target from `useReplyTargetStore`
 *   - Edit target from `useEditTargetStore` (seeds draft text)
 *   - Mention picker path → `/[workspace]/mention-picker?mode=comment`
 */
import { useCallback, useEffect, useState } from "react";
import { Alert } from "react-native";
import {
  useCreateComment,
  useEditComment,
} from "@/data/mutations/issues";
import { useReplyTargetStore } from "@/data/stores/reply-target-store";
import { useEditTargetStore } from "@/data/stores/edit-target-store";
import { useWorkspaceStore } from "@/data/workspace-store";
import { MessageComposer } from "@/components/composer/message-composer";

export function InlineCommentComposer({ issueId }: { issueId: string }) {
  const createComment = useCreateComment(issueId);
  const editComment = useEditComment(issueId);
  const wsSlug = useWorkspaceStore((s) => s.currentWorkspaceSlug);
  const replyTarget = useReplyTargetStore((s) => s.target);
  const clearReplyTarget = useReplyTargetStore((s) => s.clear);
  const editTarget = useEditTargetStore((s) => s.target);
  const clearEditTarget = useEditTargetStore((s) => s.clear);

  const [editDraft, setEditDraft] = useState("");

  // Seed the controlled draft whenever a new edit target lands.
  useEffect(() => {
    if (editTarget) {
      setEditDraft(editTarget.content);
    }
  }, [editTarget?.commentId, editTarget?.content]);

  const onSubmit = useCallback(
    async ({
      content,
      attachmentIds,
    }: {
      content: string;
      attachmentIds: string[];
    }) => {
      try {
        if (editTarget) {
          await editComment.mutateAsync({
            commentId: editTarget.commentId,
            content,
            attachmentIds:
              attachmentIds.length > 0
                ? attachmentIds
                : editTarget.attachmentIds.length > 0
                  ? editTarget.attachmentIds
                  : undefined,
          });
          clearEditTarget();
          setEditDraft("");
          return;
        }
        await createComment.mutateAsync({
          content,
          parentId: replyTarget?.commentId,
          attachmentIds: attachmentIds.length > 0 ? attachmentIds : undefined,
        });
      } catch (err) {
        if (editTarget) {
          Alert.alert(
            "Could not save edit",
            err instanceof Error ? err.message : "Unknown error",
          );
        }
        // Rethrow so MessageComposer's catch path restores text + chips.
        throw err;
      }
    },
    [
      createComment,
      editComment,
      replyTarget?.commentId,
      editTarget,
      clearEditTarget,
    ],
  );

  const clearComposerTarget = useCallback(() => {
    clearReplyTarget();
    clearEditTarget();
    setEditDraft("");
  }, [clearReplyTarget, clearEditTarget]);

  const isEditing = !!editTarget;

  return (
    <MessageComposer
      onSubmit={onSubmit}
      mentionPickerPath={{
        pathname: "/[workspace]/mention-picker",
        params: { workspace: wsSlug ?? "", mode: "comment" },
      }}
      uploadContext={{ issueId }}
      placeholder={isEditing ? "Edit comment…" : "Add a comment…"}
      pillLabel={isEditing ? "Edit comment…" : "Add a comment, @ to mention…"}
      pillIcon="chatbubble-ellipses-outline"
      value={isEditing ? editDraft : undefined}
      onChangeText={isEditing ? setEditDraft : undefined}
      replyTarget={
        isEditing
          ? {
              actorName: "Editing",
              preview: editTarget.content,
            }
          : replyTarget
            ? {
                actorName: replyTarget.actorName,
                preview: replyTarget.preview,
              }
            : null
      }
      onClearReplyTarget={clearComposerTarget}
      expandTrigger={
        editTarget?.commentId
          ? `edit:${editTarget.commentId}`
          : (replyTarget?.commentId ?? null)
      }
    />
  );
}
