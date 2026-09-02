import QtQuick 2.5
import QtQuick.Layouts 1.0

ToolbarTool {
    id: root
    objectName: "shareMenu"

    property bool forceStackView: false

    foldoutContent: ColumnLayout {
        objectName: "shareMenuContent"
        spacing: 0

        ScreenShareButton {
            id: screenShareButton
            toolbar: root.toolbar
        }
        SendByEmailButton {
            id: sendByEmailButton
            toolbar: root.toolbar
        }
        ShareByUrlButton {
            id: shareByUrlButton
            toolbar: root.toolbar
        }
    }
}
