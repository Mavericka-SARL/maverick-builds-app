<#ftl output_format="plainText">
<#assign kind = (user.attributes.mvx_invitation)!"">
<#assign by = (user.attributes.mvx_invited_by)!"">
<#assign workspace = (user.attributes.mvx_workspace)!"">
<#if kind == "signup">
${msg("mvxSignupLead", realmName)}
<#elseif kind != "invite" || !by?has_content>
${msg("mvxAccountLead", realmName)}
<#elseif workspace?has_content>
${msg("mvxInviteLead", by, workspace, realmName)}
<#else>
${msg("mvxInviteNoWorkspaceLead", by, realmName)}
</#if>

${link}

${msg("mvxLinkWorks", linkExpirationFormatter(linkExpiration))} ${msg((kind == "signup")?then("mvxSignupIgnore", "mvxInviteIgnore"))}
