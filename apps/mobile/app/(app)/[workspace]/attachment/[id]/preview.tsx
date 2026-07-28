import { useCallback, useState } from "react";
import {
  ActivityIndicator,
  Platform,
  Pressable,
  ScrollView,
  View,
} from "react-native";
import { Stack, useLocalSearchParams } from "expo-router";
import { Ionicons } from "@expo/vector-icons";
import { useQuery } from "@tanstack/react-query";
import {
  PreviewTooLargeError,
  PreviewUnsupportedError,
} from "@/data/api";
import {
  attachmentContentOptions,
  attachmentDetailOptions,
} from "@/data/queries/attachments";
import { useWorkspaceStore } from "@/data/workspace-store";
import { Button } from "@/components/ui/button";
import { Text } from "@/components/ui/text";
import { Markdown } from "@/lib/markdown";
import {
  getAttachmentPreviewKind,
  htmlToStaticText,
} from "@/lib/attachment-preview";
import { downloadAndOpenAttachment } from "@/lib/download-attachment";
import { useColorScheme } from "@/lib/use-color-scheme";
import { THEME } from "@/lib/theme";

export default function AttachmentPreviewRoute() {
  const { id } = useLocalSearchParams<{ id: string }>();
  const { colorScheme } = useColorScheme();
  const theme = THEME[colorScheme];
  const workspaceId = useWorkspaceStore((state) => state.currentWorkspaceId);
  const metadata = useQuery(attachmentDetailOptions(workspaceId, id));
  const attachment = metadata.data;
  const kind = attachment
    ? getAttachmentPreviewKind(attachment.content_type, attachment.filename)
    : null;
  const isTextBacked =
    kind === "markdown" || kind === "html" || kind === "text";
  const content = useQuery(
    attachmentContentOptions(workspaceId, id, isTextBacked),
  );
  const [downloading, setDownloading] = useState(false);

  const download = useCallback(async () => {
    if (!attachment?.id || downloading) return;
    setDownloading(true);
    try {
      await downloadAndOpenAttachment(
        attachment.id,
        attachment.filename || "download",
      );
    } finally {
      setDownloading(false);
    }
  }, [attachment?.id, attachment?.filename, downloading]);

  return (
    <View className="flex-1 bg-background">
      <Stack.Screen
        options={{
          title: attachment?.filename || "Attachment preview",
          headerBackTitle: "Back",
          headerRight: () => (
            <Pressable
              onPress={() => void download()}
              disabled={!attachment?.id || downloading}
              hitSlop={10}
              accessibilityRole="button"
              accessibilityLabel="Download attachment"
              className="h-9 w-9 items-center justify-center disabled:opacity-40"
            >
              {downloading ? (
                <ActivityIndicator size="small" color={theme.foreground} />
              ) : (
                <Ionicons
                  name="download-outline"
                  size={21}
                  color={theme.foreground}
                />
              )}
            </Pressable>
          ),
        }}
      />

      {metadata.isLoading ? (
        <CenteredMessage loading message="Loading attachment…" />
      ) : metadata.error || !attachment?.id ? (
        <DownloadFallback
          message="Could not load attachment metadata."
          onDownload={() => void download()}
          disabled={!attachment?.id || downloading}
          downloading={downloading}
        />
      ) : !isTextBacked ? (
        <DownloadFallback
          message="This file type is not available for in-app preview."
          onDownload={() => void download()}
          disabled={downloading}
          downloading={downloading}
        />
      ) : content.isLoading ? (
        <CenteredMessage loading message="Loading preview…" />
      ) : content.error ? (
        <DownloadFallback
          message={previewErrorMessage(content.error)}
          onDownload={() => void download()}
          disabled={downloading}
          downloading={downloading}
        />
      ) : content.data ? (
        <PreviewBody kind={kind} text={content.data.text} />
      ) : null}
    </View>
  );
}

function PreviewBody({
  kind,
  text,
}: {
  kind: "markdown" | "html" | "text";
  text: string;
}) {
  const { colorScheme } = useColorScheme();
  const theme = THEME[colorScheme];

  if (kind === "html") {
    const staticText = htmlToStaticText(text);
    return (
      <ScrollView
        showsVerticalScrollIndicator={false}
        contentContainerStyle={{ padding: 16 }}
      >
        <View className="gap-3">
          <Text className="text-xs text-muted-foreground">
            Static HTML preview. Scripts, links, frames, and remote resources
            are disabled.
          </Text>
          <Text
            selectable
            style={{
              color: theme.foreground,
              fontSize: 14,
              lineHeight: 22,
            }}
          >
            {staticText || "No readable text content."}
          </Text>
        </View>
      </ScrollView>
    );
  }

  if (kind === "markdown") {
    return (
      <ScrollView
        showsVerticalScrollIndicator={false}
        contentContainerStyle={{ padding: 16 }}
      >
        <Markdown content={text} />
      </ScrollView>
    );
  }

  return (
    <ScrollView
      horizontal
      showsHorizontalScrollIndicator
      contentContainerStyle={{ minWidth: "100%" }}
    >
      <ScrollView
        showsVerticalScrollIndicator
        contentContainerStyle={{ padding: 16 }}
      >
        <Text
          selectable
          style={{
            color: theme.foreground,
            fontFamily: Platform.select({ ios: "Menlo", android: "monospace" }),
            fontSize: 13,
            lineHeight: 19,
          }}
        >
          {text}
        </Text>
      </ScrollView>
    </ScrollView>
  );
}

function previewErrorMessage(error: Error): string {
  if (error instanceof PreviewTooLargeError) {
    return "This file is larger than the 2 MB inline-preview limit.";
  }
  if (error instanceof PreviewUnsupportedError) {
    return "This file type is not supported for inline preview.";
  }
  return "Preview failed. You can still download the attachment.";
}

function CenteredMessage({
  loading,
  message,
}: {
  loading?: boolean;
  message: string;
}) {
  return (
    <View className="flex-1 items-center justify-center gap-3 px-8">
      {loading ? <ActivityIndicator /> : null}
      <Text className="text-sm text-muted-foreground text-center">
        {message}
      </Text>
    </View>
  );
}

function DownloadFallback({
  message,
  onDownload,
  disabled,
  downloading = false,
}: {
  message: string;
  onDownload: () => void;
  disabled: boolean;
  downloading?: boolean;
}) {
  const { colorScheme } = useColorScheme();
  const theme = THEME[colorScheme];
  return (
    <View className="flex-1 items-center justify-center gap-4 px-8">
      <Ionicons
        name="document-text-outline"
        size={36}
        color={theme.mutedForeground}
      />
      <Text className="text-sm text-muted-foreground text-center">
        {message}
      </Text>
      <Button variant="outline" onPress={onDownload} disabled={disabled}>
        {downloading ? (
          <ActivityIndicator size="small" color={theme.foreground} />
        ) : (
          <Ionicons
            name="download-outline"
            size={18}
            color={theme.foreground}
          />
        )}
        <Text>{downloading ? "Downloading…" : "Download"}</Text>
      </Button>
    </View>
  );
}
