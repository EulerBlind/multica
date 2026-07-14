import { queryOptions } from "@tanstack/react-query";
import { api } from "@/data/api";

export const attachmentKeys = {
  all: (workspaceId: string | null) => ["attachments", workspaceId] as const,
  detail: (workspaceId: string | null, id: string) =>
    [...attachmentKeys.all(workspaceId), "detail", id] as const,
  content: (workspaceId: string | null, id: string) =>
    [...attachmentKeys.all(workspaceId), "content", id] as const,
};

/** Fresh metadata re-signs `download_url` before the preview header exposes
 * its download fallback. */
export const attachmentDetailOptions = (
  workspaceId: string | null,
  id: string,
) =>
  queryOptions({
    queryKey: attachmentKeys.detail(workspaceId, id),
    queryFn: ({ signal }) => api.getAttachment(id, { signal }),
    enabled: !!workspaceId && !!id,
  });

/** The server marks preview bodies no-store because workspace access can be
 * revoked. Keep the same posture on mobile by forcing a fresh request each
 * time the preview screen mounts. */
export const attachmentContentOptions = (
  workspaceId: string | null,
  id: string,
  enabled: boolean,
) =>
  queryOptions({
    queryKey: attachmentKeys.content(workspaceId, id),
    queryFn: ({ signal }) => api.getAttachmentTextContent(id, { signal }),
    enabled: !!workspaceId && !!id && enabled,
    staleTime: 0,
    gcTime: 0,
    retry: false,
  });
