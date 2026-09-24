/** A Baidu InfoFlow (如流) robot installation bound to a single Multica agent.
 *
 * Wire shape mirrors `InfoFlowInstallationResponse` in
 * `server/internal/handler/infoflow.go`. InfoFlow uses the bring-your-own-app
 * model: an agent owner or workspace admin registers a robot in the InfoFlow
 * open platform and pastes its application id, AppKey and AppSecret, which the
 * backend validates before persisting.
 *
 * New fields the backend adds in the future MUST default to optional so older
 * desktop builds keep parsing the response — see CLAUDE.md → API Response
 * Compatibility. */
export interface InfoFlowInstallation {
  id: string;
  workspace_id: string;
  agent_id: string;
  /** The InfoFlow open-platform application id. Doubles as the routing key
   * that maps an inbound event to its installation. */
  application_id: string;
  /** The robot's own InfoFlow user id, used to suppress the bot's own
   * outbound messages on the inbound path. Absent when the operator did not
   * supply it at install time. */
  bot_user_id?: string;
  installer_user_id: string;
  status: "active" | "revoked" | string;
  installed_at: string;
  created_at: string;
  updated_at: string;
}

export interface ListInfoFlowInstallationsResponse {
  installations: InfoFlowInstallation[];
  /** Whether the deployment has the at-rest secret key
   * (`MULTICA_INFOFLOW_SECRET_KEY`) configured. When false the connect entry
   * points are hidden and the panel renders an "ask the operator to enable
   * InfoFlow" state. */
  configured: boolean;
  /** Whether the install path is available. Kept as a separate flag for
   * forward/backward compat; optional so an older desktop build that predates
   * it treats it as off. */
  install_supported?: boolean;
}

/** Request body for a bring-your-own-app install: the application id, AppKey
 * and AppSecret an agent owner or workspace admin pastes from the InfoFlow
 * open platform. `bot_user_id` is optional and only needed so the adapter can
 * ignore the robot's own messages. */
export interface RegisterInfoFlowBYORequest {
  application_id: string;
  app_key: string;
  app_secret: string;
  bot_user_id?: string;
}

/** Post-redemption echo: the InfoFlow user id the token carried is now bound
 * to the logged-in Multica user in this workspace/installation. */
export interface RedeemInfoFlowBindingTokenResponse {
  workspace_id: string;
  installation_id: string;
  infoflow_user_id: string;
}
