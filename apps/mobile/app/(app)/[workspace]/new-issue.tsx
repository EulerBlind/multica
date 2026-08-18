/**
 * New issue creation modal — manual only.
 *
 * Layout follows Apple Reminders / Linear iOS / Things 3: one vertical
 * scrolling form (title → description → property chips), no sticky bottom
 * toolbar. Property chips are part of the form, not pinned above keyboard.
 * MentionSuggestionBar floats above keyboard only when the user is mid-@.
 *
 * Attachments use the same camera / media / file upload state as the
 * comment composer. Only completed attachment ids enter the create payload.
 *
 * Mention pipeline shares `useMentionInput` with `issue/[id]/new-comment.tsx`
 * — both surfaces produce canonical `[@name](mention://type/id)` markdown
 * recognised by util.ParseMentions on the server.
 */
import { useCallback, useEffect, useState } from "react";
import { KeyboardAvoidingView } from "react-native-keyboard-controller";
import {
  Alert,
  ScrollView,
  TextInput,
  View,
} from "react-native";
import { Stack, router } from "expo-router";
import { SubmitIssueButton } from "@/components/issue/submit-issue-button";
import { CreateFormAttributeRow } from "@/components/issue/create-form-attribute-row";
import { MentionSuggestionBar } from "@/components/issue/mention-suggestion-bar";
import { DescriptionField } from "@/components/issue/description-field";
import { MOBILE_PLACEHOLDER_COLOR } from "@/components/ui/input-tokens";
import { IconButton } from "@/components/ui/icon-button";
import { ComposerAttachmentRow } from "@/components/issue/composer-attachment-row";
import { useAttachmentUploads } from "@/components/issue/use-attachment-uploads";
import { useCreateIssue } from "@/data/mutations/issues";
import { useNewIssueDraftStore } from "@/data/stores/new-issue-draft-store";
import { useMentionInput } from "@/lib/use-mention-input";

export default function NewIssueModal() {
  const [title, setTitle] = useState("");
  const description = useMentionInput();
  // Attribute chips (status / priority / assignee / due date / project)
  // live in `useNewIssueDraftStore` so the new-issue-picker/* formSheet
  // routes can read and write the same values without a parent-child
  // React relationship. The store is reset on mount + on unmount so
  // re-opening the new-issue modal starts clean.
  const status = useNewIssueDraftStore((s) => s.status);
  const priority = useNewIssueDraftStore((s) => s.priority);
  const assignee = useNewIssueDraftStore((s) => s.assignee);
  const dueDate = useNewIssueDraftStore((s) => s.dueDate);
  const project = useNewIssueDraftStore((s) => s.project);
  const resetDraft = useNewIssueDraftStore((s) => s.reset);
  const {
    attachments,
    completedIds,
    hasInFlightUpload,
    chooseImage,
    takePhoto,
    chooseFile,
    removeAttachment,
    retryAttachment,
    markCommitted,
  } = useAttachmentUploads();

  useEffect(() => {
    resetDraft();
    return () => {
      resetDraft();
    };
  }, [resetDraft]);

  const createIssue = useCreateIssue();
  const isSubmitting = createIssue.isPending;

  const canSubmit =
    !isSubmitting && !hasInFlightUpload && title.trim().length > 0;

  const onSubmit = useCallback(async () => {
    const trimmedTitle = title.trim();
    if (trimmedTitle.length === 0) return;
    const finalDescription = description.serialize().trim();
    try {
      await createIssue.mutateAsync({
        title: trimmedTitle,
        description: finalDescription || undefined,
        status,
        priority,
        ...(assignee
          ? { assignee_type: assignee.type, assignee_id: assignee.id }
          : {}),
        ...(dueDate ? { due_date: dueDate } : {}),
        ...(project ? { project_id: project.id } : {}),
        ...(completedIds.length > 0 ? { attachment_ids: completedIds } : {}),
      });
      markCommitted(completedIds);
      router.back();
    } catch (err) {
      Alert.alert(
        "Failed to create issue",
        err instanceof Error ? err.message : "Unknown error",
      );
    }
  }, [
    title,
    description,
    status,
    priority,
    assignee,
    dueDate,
    project,
    completedIds,
    markCommitted,
    createIssue,
  ]);

  const headerRight = useCallback(
    () => (
      <SubmitIssueButton
        disabled={!canSubmit}
        loading={isSubmitting}
        onPress={onSubmit}
      />
    ),
    [canSubmit, isSubmitting, onSubmit],
  );

  return (
    <>
      <Stack.Screen options={{ headerRight }} />
      <KeyboardAvoidingView
        className="flex-1 bg-background"
        behavior="padding"
      >
        <ScrollView
          className="flex-1"
          contentContainerClassName="px-4 pt-4 pb-6 gap-4"
          keyboardShouldPersistTaps="handled"
        >
          <TextInput
            value={title}
            onChangeText={setTitle}
            placeholder="Issue title"
            placeholderTextColor={MOBILE_PLACEHOLDER_COLOR}
            className="text-2xl font-semibold text-foreground py-2"
            autoFocus
            returnKeyType="next"
            editable={!isSubmitting}
          />
          <DescriptionField
            description={description}
            disabled={isSubmitting}
          />
          <View className="gap-2 rounded-xl border border-border bg-secondary/40 p-3">
            <ComposerAttachmentRow
              mentions={[]}
              attachments={attachments}
              onRemoveMention={() => {}}
              onRemoveAttachment={removeAttachment}
              onRetryAttachment={retryAttachment}
            />
            <View className="flex-row items-center gap-2">
              <IconButton
                name="camera-outline"
                onPress={() => void takePhoto()}
                disabled={isSubmitting}
                accessibilityLabel="Take photo"
              />
              <IconButton
                name="image-outline"
                onPress={() => void chooseImage()}
                disabled={isSubmitting}
                accessibilityLabel="Choose image"
              />
              <IconButton
                name="attach-outline"
                onPress={() => void chooseFile()}
                disabled={isSubmitting}
                accessibilityLabel="Choose file"
              />
            </View>
          </View>
          <CreateFormAttributeRow />
        </ScrollView>

        {/* Mention suggestions float above the keyboard only when the user
            types `@`. Self-hides via `if (!visible) return null` so it
            doesn't take space at rest. */}
        <MentionSuggestionBar {...description.suggestionBar} />
      </KeyboardAvoidingView>
    </>
  );
}
