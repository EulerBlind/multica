/**
 * Description block. Renders markdown via the standalone mobile markdown
 * renderer at apps/mobile/lib/markdown/. Empty / null descriptions show
 * a muted "No description." placeholder rather than collapsing the block,
 * so the layout above the timeline stays stable when the user adds a
 * description later.
 *
 * Attachments are fetched per-issue so markdown can resolve `mc://file/<id>`
 * image URIs into real `download_url` HTTPS endpoints — without this the
 * iOS image loader doesn't understand the mc: scheme and the image fails.
 * TanStack Query dedupes the request across this component and CommentCard
 * (both call `issueAttachmentsOptions(wsId, issueId)`), so only one
 * network roundtrip fires per issue.
 */
import { View } from "react-native";
import { useQuery } from "@tanstack/react-query";
import { Text } from "@/components/ui/text";
import { CommentAttachmentList } from "@/components/issue/comment-attachment-list";
import { Markdown } from "@/lib/markdown";
import { issueOwnedAttachments } from "@/lib/attachment-dedup";
import { issueAttachmentsOptions } from "@/data/queries/issues";
import { useWorkspaceStore } from "@/data/workspace-store";

export function IssueDescription({
  issueId,
  description,
}: {
  issueId: string;
  description: string | null;
}) {
  const wsId = useWorkspaceStore((s) => s.currentWorkspaceId);
  const { data: attachments } = useQuery(
    issueAttachmentsOptions(wsId, issueId),
  );
  const persistedAttachments = issueOwnedAttachments(attachments, issueId);
  const hasDescription = !!description?.trim();

  return (
    <View className="px-4 pb-4 gap-3">
      {hasDescription ? (
        <Markdown content={description ?? ""} attachments={attachments} />
      ) : (
        <Text className="text-sm text-muted-foreground italic">
          No description.
        </Text>
      )}
      <CommentAttachmentList
        attachments={persistedAttachments}
        content={description ?? undefined}
      />
    </View>
  );
}
