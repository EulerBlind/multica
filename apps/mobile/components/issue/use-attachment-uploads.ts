import { useCallback, useEffect, useRef, useState } from "react";
import { Alert, Linking } from "react-native";
import * as DocumentPicker from "expo-document-picker";
import * as ImagePicker from "expo-image-picker";
import { api, MAX_FILE_SIZE, type FileAsset } from "@/data/api";
import type { ComposerAttachmentItem } from "./composer-attachment-row";
import { completedAttachmentIds } from "@/lib/attachment-uploads";

const UPLOAD_TIMEOUT_MS = 180_000;

export interface AttachmentUploadContext {
  issueId?: string;
  commentId?: string;
}

function makeLocalId(): string {
  return `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`;
}

export function useAttachmentUploads(context?: AttachmentUploadContext) {
  const [attachments, setAttachmentsState] = useState<
    ComposerAttachmentItem[]
  >([]);
  const attachmentsRef = useRef<ComposerAttachmentItem[]>([]);
  const committedIdsRef = useRef(new Set<string>());
  const uncommittedIdsRef = useRef(new Set<string>());
  const controllersRef = useRef(new Map<string, AbortController>());
  const pendingResultHandledRef = useRef(false);

  const setAttachments = useCallback(
    (
      next:
        | ComposerAttachmentItem[]
        | ((current: ComposerAttachmentItem[]) => ComposerAttachmentItem[]),
    ) => {
      const value =
        typeof next === "function" ? next(attachmentsRef.current) : next;
      attachmentsRef.current = value;
      setAttachmentsState(value);
    },
    [],
  );

  const deleteUncommitted = useCallback((id: string) => {
    if (committedIdsRef.current.has(id)) return;
    uncommittedIdsRef.current.delete(id);
    void api.deleteAttachment(id).catch((error) => {
      console.warn("[attachment] cleanup failed", {
        attachmentId: id,
        error: error instanceof Error ? error.message : "Unknown error",
      });
    });
  }, []);

  const startUpload = useCallback(
    async (localId: string, asset: FileAsset) => {
      const controller = new AbortController();
      controllersRef.current.get(localId)?.abort();
      controllersRef.current.set(localId, controller);
      const timeoutId = setTimeout(
        () => controller.abort(new Error("Upload timed out")),
        UPLOAD_TIMEOUT_MS,
      );

      try {
        const result = await api.uploadFile(asset, {
          ...context,
          signal: controller.signal,
        });
        uncommittedIdsRef.current.add(result.id);
        if (controller.signal.aborted) {
          deleteUncommitted(result.id);
          return;
        }
        setAttachments((current) =>
          current.map((item) =>
            item.localId === localId
              ? {
                  ...item,
                  status: "completed",
                  id: result.id,
                  url: result.url,
                  downloadUrl: result.download_url,
                  error: undefined,
                }
              : item,
          ),
        );
      } catch (error) {
        if (
          controller.signal.aborted &&
          !attachmentsRef.current.some((item) => item.localId === localId)
        ) {
          return;
        }
        setAttachments((current) =>
          current.map((item) =>
            item.localId === localId
              ? {
                  ...item,
                  status: "failed",
                  error:
                    controller.signal.aborted
                      ? "Upload timed out"
                      : error instanceof Error
                        ? error.message
                        : "Unknown error",
                }
              : item,
          ),
        );
      } finally {
        clearTimeout(timeoutId);
        if (controllersRef.current.get(localId) === controller) {
          controllersRef.current.delete(localId);
        }
      }
    },
    [context, deleteUncommitted, setAttachments],
  );

  const queueAsset = useCallback(
    (asset: FileAsset & { size?: number }) => {
      if (asset.size != null && asset.size > MAX_FILE_SIZE) {
        Alert.alert("File too large", "Files must be smaller than 100 MB.");
        return;
      }
      const localId = makeLocalId();
      setAttachments((current) => [
        ...current,
        {
          localId,
          localUri: asset.uri,
          filename: asset.name,
          mimeType: asset.type,
          status: "uploading",
        },
      ]);
      void startUpload(localId, asset);
    },
    [setAttachments, startUpload],
  );

  const queueImageResult = useCallback(
    (result: ImagePicker.ImagePickerResult) => {
      if (result.canceled) return;
      const picked = result.assets[0];
      if (!picked) return;
      queueAsset({
        uri: picked.uri,
        name: picked.fileName ?? `image-${Date.now()}.jpg`,
        type: picked.mimeType ?? "image/jpeg",
        size: picked.fileSize,
      });
    },
    [queueAsset],
  );

  const chooseImage = useCallback(async () => {
    try {
      queueImageResult(
        await ImagePicker.launchImageLibraryAsync({
          mediaTypes: ImagePicker.MediaTypeOptions.Images,
          quality: 1,
        }),
      );
    } catch (error) {
      Alert.alert(
        "Photo library unavailable",
        error instanceof Error
          ? error.message
          : "Unable to open the photo library.",
      );
    }
  }, [queueImageResult]);

  const takePhoto = useCallback(async () => {
    try {
      const permission = await ImagePicker.requestCameraPermissionsAsync();
      if (!permission.granted) {
        Alert.alert(
          "Camera permission needed",
          "Allow camera access to take a photo for this attachment.",
          permission.canAskAgain
            ? [{ text: "OK" }]
            : [
                { text: "Cancel", style: "cancel" },
                {
                  text: "Open Settings",
                  onPress: () => void Linking.openSettings(),
                },
              ],
        );
        return;
      }
      queueImageResult(
        await ImagePicker.launchCameraAsync({
          mediaTypes: ImagePicker.MediaTypeOptions.Images,
          quality: 1,
        }),
      );
    } catch (error) {
      Alert.alert(
        "Camera unavailable",
        error instanceof Error ? error.message : "Unable to open the camera.",
      );
    }
  }, [queueImageResult]);

  const chooseFile = useCallback(async () => {
    try {
      const result = await DocumentPicker.getDocumentAsync({
        type: "*/*",
        copyToCacheDirectory: true,
      });
      if (result.canceled) return;
      const picked = result.assets[0];
      if (!picked) return;
      queueAsset({
        uri: picked.uri,
        name: picked.name,
        type: picked.mimeType ?? "application/octet-stream",
        size: picked.size,
      });
    } catch (error) {
      Alert.alert(
        "File picker unavailable",
        error instanceof Error ? error.message : "Unable to open the file picker.",
      );
    }
  }, [queueAsset]);

  const removeAttachment = useCallback(
    (localId: string) => {
      controllersRef.current.get(localId)?.abort();
      controllersRef.current.delete(localId);
      const item = attachmentsRef.current.find(
        (candidate) => candidate.localId === localId,
      );
      setAttachments((current) =>
        current.filter((candidate) => candidate.localId !== localId),
      );
      if (item?.id) deleteUncommitted(item.id);
    },
    [deleteUncommitted, setAttachments],
  );

  const retryAttachment = useCallback(
    (localId: string) => {
      const item = attachmentsRef.current.find(
        (candidate) => candidate.localId === localId,
      );
      if (!item) return;
      setAttachments((current) =>
        current.map((candidate) =>
          candidate.localId === localId
            ? { ...candidate, status: "uploading", error: undefined }
            : candidate,
        ),
      );
      void startUpload(localId, {
        uri: item.localUri,
        name: item.filename,
        type: item.mimeType,
      });
    },
    [setAttachments, startUpload],
  );

  const markCommitted = useCallback((ids: string[]) => {
    ids.forEach((id) => {
      committedIdsRef.current.add(id);
      uncommittedIdsRef.current.delete(id);
    });
  }, []);

  useEffect(() => {
    if (pendingResultHandledRef.current) return;
    pendingResultHandledRef.current = true;
    void ImagePicker.getPendingResultAsync()
      .then((result) => {
        if (!result) return;
        if ("code" in result) {
          console.warn("[attachment] pending picker returned an error", {
            code: result.code,
            message: result.message,
          });
          return;
        }
        queueImageResult(result);
      })
      .catch((error) => {
        console.warn("[attachment] pending picker result failed", {
          error: error instanceof Error ? error.message : "Unknown error",
        });
      });
  }, [queueImageResult]);

  useEffect(() => {
    const controllers = controllersRef.current;
    const uncommittedIds = uncommittedIdsRef.current;
    return () => {
      controllers.forEach((controller) => controller.abort());
      for (const id of uncommittedIds) {
        deleteUncommitted(id);
      }
    };
  }, [deleteUncommitted]);

  return {
    attachments,
    setAttachments,
    completedIds: completedAttachmentIds(attachments),
    hasInFlightUpload: attachments.some((item) => item.status === "uploading"),
    chooseImage,
    takePhoto,
    chooseFile,
    removeAttachment,
    retryAttachment,
    markCommitted,
  };
}
