interface AttachmentWithStatus {
  status: "uploading" | "completed" | "failed";
  id?: string;
}

export function completedAttachmentIds(items: AttachmentWithStatus[]): string[] {
  return items
    .filter((item) => item.status === "completed")
    .map((item) => item.id)
    .filter((id): id is string => Boolean(id));
}
