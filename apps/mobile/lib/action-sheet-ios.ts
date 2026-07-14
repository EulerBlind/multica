import { ActionSheetIOS } from "react-native";
import { getActionSheetIndexes, type ActionSheetRequest } from "./action-sheet";

export function presentIOSActionSheet(request: ActionSheetRequest): void {
  ActionSheetIOS.showActionSheetWithOptions(
    {
      options: request.items.map((item) => item.label),
      title: request.title,
      message: request.message,
      ...getActionSheetIndexes(request.items),
    },
    (index) => {
      const item = request.items[index];
      if (!item || item.disabled) return;
      item.onPress();
    },
  );
}
