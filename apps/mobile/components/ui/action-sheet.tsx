import {
  Modal,
  Platform,
  Pressable,
  ScrollView,
} from "react-native";
import {
  createContext,
  useCallback,
  useContext,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { useSafeAreaInsets } from "react-native-safe-area-context";
import type { ActionSheetRequest } from "@/lib/action-sheet";
import { presentIOSActionSheet } from "@/lib/action-sheet-ios";
import { Text } from "@/components/ui/text";

type ShowActionSheet = (request: ActionSheetRequest) => void;

const ActionSheetContext = createContext<ShowActionSheet | null>(null);

export function ActionSheetProvider({ children }: { children: ReactNode }) {
  const insets = useSafeAreaInsets();
  const [request, setRequest] = useState<ActionSheetRequest | null>(null);
  const closingRef = useRef(false);

  const showActionSheet = useCallback<ShowActionSheet>((next) => {
    if (Platform.OS === "ios") {
      presentIOSActionSheet(next);
      return;
    }

    closingRef.current = false;
    setRequest(next);
  }, []);

  const dismiss = useCallback((itemIndex?: number) => {
    if (closingRef.current) return;
    closingRef.current = true;
    const resolvedIndex =
      itemIndex ?? request?.items.findIndex((item) => item.role === "cancel");
    const item =
      resolvedIndex == null || resolvedIndex < 0
        ? undefined
        : request?.items[resolvedIndex];
    setRequest(null);

    if (item && !item.disabled) {
      // Let Android remove the first Modal before an action presents another
      // sheet or Alert. This keeps nested reaction menus deterministic.
      setTimeout(item.onPress, 0);
    }
  }, [request]);

  return (
    <ActionSheetContext.Provider value={showActionSheet}>
      {children}
      <Modal
        transparent
        visible={request !== null}
        animationType="fade"
        onRequestClose={() => dismiss()}
        statusBarTranslucent
      >
        <Pressable
          className="flex-1 justify-end bg-black/45"
          onPress={() => dismiss()}
          accessibilityRole="button"
          accessibilityLabel="Close action menu"
        >
          <Pressable
            className="max-h-[75%] rounded-t-3xl bg-background px-3 pt-3"
            style={{ paddingBottom: Math.max(insets.bottom, 12) }}
            onPress={(event) => event.stopPropagation()}
          >
            {request?.title ? (
              <Text className="px-3 pb-1 text-center text-base font-semibold text-foreground">
                {request.title}
              </Text>
            ) : null}
            {request?.message ? (
              <Text className="px-3 pb-2 text-center text-sm text-muted-foreground">
                {request.message}
              </Text>
            ) : null}
            <ScrollView showsVerticalScrollIndicator={false}>
              {request?.items.map((item, index) => (
                <Pressable
                  key={item.key}
                  disabled={item.disabled}
                  onPress={() => dismiss(index)}
                  accessibilityRole="button"
                  accessibilityState={{ disabled: item.disabled }}
                  className="min-h-12 items-center justify-center border-b border-border px-4 active:bg-secondary"
                >
                  <Text
                    className={
                      item.role === "destructive"
                        ? "text-base font-medium text-destructive"
                        : item.disabled
                          ? "text-base text-muted-foreground opacity-50"
                          : "text-base font-medium text-foreground"
                    }
                  >
                    {item.label}
                  </Text>
                </Pressable>
              ))}
            </ScrollView>
          </Pressable>
        </Pressable>
      </Modal>
    </ActionSheetContext.Provider>
  );
}

export function useActionSheet(): ShowActionSheet {
  const value = useContext(ActionSheetContext);
  if (!value) {
    throw new Error("useActionSheet must be used inside ActionSheetProvider");
  }
  return value;
}
